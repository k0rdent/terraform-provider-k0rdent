package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The platform answers a user's SSH keys only as references to
// `ssh_public_key` credentials, never as the keys. Each credential's metadata
// carries the key's SHA-256 fingerprint, readable by an account with
// `credentials.metadata.read` (among the built-in roles, `credentials-admin`).
//
// A create holds the fingerprints of the keys it sent. Anything else that puts
// a resource in state, an import above all, holds none, so a refresh reads
// them from the platform for each user with references and none held. A plan
// then checks the configured keys against them, and fails for a user it can't
// check: a changed key that planned no change would leave the old key on the
// nodes with nobody told.

// sshKeysUncheckedKey holds, by username, why the last refresh couldn't read
// that user's fingerprints, and sshKeysRevokedKey the users with a key whose
// credential the platform has revoked or is revoking.
const (
	sshKeysUncheckedKey = "ssh_keys_unchecked"
	sshKeysRevokedKey   = "ssh_keys_revoked"
)

// holdPlatformFingerprints reads each user's keys as the platform holds them:
// their fingerprints, which replace any held, and whether one was revoked. A
// user it can't read keeps what's held. One with none held is recorded with
// the reason, for the plan's error, and on an import that's also a warning,
// since the import itself succeeds.
func holdPlatformFingerprints(ctx context.Context, meta *Meta, private interface {
	privateState
	privateWriter
}, access types.Set, imported bool) diag.Diagnostics {
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) ||
		meta == nil || meta.Client == nil || access.IsNull() || access.IsUnknown() {
		return nil
	}
	var users []sshUserModel
	if access.ElementsAs(ctx, &users, false).HasError() {
		return nil
	}
	held := loadFingerprints(ctx, private)
	if held == nil {
		held = map[string][]string{}
	}
	unchecked := map[string]string{}
	revoked := []string{}
	for _, u := range users {
		name := u.Username.ValueString()
		var refs []string
		if u.Credentials.IsNull() || u.Credentials.IsUnknown() ||
			u.Credentials.ElementsAs(ctx, &refs, false).HasError() || len(refs) == 0 {
			continue
		}
		fps, gone, why := readFingerprints(ctx, meta, refs)
		if gone {
			revoked = append(revoked, name)
		}
		switch {
		case why != "":
			if _, ok := held[name]; !ok {
				unchecked[name] = why
			}
		default:
			held[name] = fps
		}
	}
	slices.Sort(revoked)
	var diags diag.Diagnostics
	diags.Append(saveFingerprints(ctx, private, held)...)
	if b, err := json.Marshal(unchecked); err == nil {
		diags.Append(private.SetKey(ctx, sshKeysUncheckedKey, b)...)
	}
	if b, err := json.Marshal(revoked); err == nil {
		diags.Append(private.SetKey(ctx, sshKeysRevokedKey, b)...)
	}
	if imported && len(unchecked) > 0 {
		diags.AddWarning("The SSH keys can't be checked",
			fmt.Sprintf("The platform answers the keys of %s only as references, and the provider couldn't "+
				"read their fingerprints: %s. An apply that configures keys for them will stop until it can, "+
				"since it couldn't tell a changed key from the one on the nodes. %s",
				strings.Join(slices.Sorted(maps.Keys(unchecked)), ", "), describeUnchecked(unchecked), keysNeedRole))
	}
	return diags
}

// readFingerprints is the fingerprint of each referenced key, in order, and
// whether any key's credential is revoked or being revoked, or why one
// couldn't be read. A revoked key keeps its fingerprint, and the nodes keep
// the key, so it's compared like any other.
func readFingerprints(ctx context.Context, meta *Meta, refs []string) (fps []string, revoked bool, why string) {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		p, ok := routeOf("getProjectCredential").params(ref)
		if !ok {
			return nil, false, fmt.Sprintf("the reference %q isn't one the provider can read", ref)
		}
		got, err := meta.Client.GetProjectCredentialWithResponse(ctx, p["region"], p["project"], p["id"])
		switch {
		case err != nil:
			return nil, false, fmt.Sprintf("reading %s failed: %v", ref, err)
		case got.JSON200 == nil:
			return nil, false, fmt.Sprintf("HTTP %d reading %s", got.StatusCode(), ref)
		}
		if credentialRevoked(got.JSON200) {
			revoked = true
		}
		fp := ""
		if a := got.JSON200.MaterialAttributes; a != nil && a.Fingerprint != nil {
			fp = *a.Fingerprint
		}
		if fp == "" {
			return nil, revoked, fmt.Sprintf("%s has no fingerprint", ref)
		}
		out = append(out, fp)
	}
	return out, revoked, ""
}

// credentialRevoked is whether a key's credential is revoked, deleted, or
// being revoked. A revocation keeps the fingerprint while it runs (kind, API
// v1.3.0-rc.5), and it can't take the key off a node that's running: the
// platform's own design says its revocation "cannot remove a key from a
// provisioned host" (the k0rdent API's own design, v1.3.0-rc.6).
func credentialRevoked(c *k0rdent.CredentialRecord) bool {
	if c.State != k0rdent.CredentialRecordStateACTIVE && c.State != k0rdent.CredentialRecordStatePENDING {
		return true
	}
	op := c.Operation
	return op != nil && op.Type != nil && op.Status != nil &&
		(*op.Type == k0rdent.CredentialOperationTypeRevocation || *op.Type == k0rdent.CredentialOperationTypeResourcerevocation) &&
		*op.Status != k0rdent.CredentialOperationStatusFailed
}

// warnRevokedKeys says which users have a key whose credential is revoked. The
// nodes still take it, and the configuration still names it, so there's
// nothing to plan: a replacement would put the same key back.
func warnRevokedKeys(diags *diag.Diagnostics, users []string) {
	if len(users) == 0 {
		return
	}
	diags.AddWarning("An SSH key's credential is revoked, and the nodes still take the key",
		fmt.Sprintf("The platform reports a revoked key for %s. A revocation can't take a key off nodes that "+
			"are running, so they still accept it. To remove the key, change it in the configuration: the "+
			"platform sets keys only when nodes are made, so the change replaces the resource.",
			strings.Join(users, ", ")))
}

// loadRevoked is sshKeysRevokedKey's users.
func loadRevoked(ctx context.Context, private privateState) []string {
	raw, diags := private.GetKey(ctx, sshKeysRevokedKey)
	if diags.HasError() || len(raw) == 0 {
		return nil
	}
	var users []string
	if json.Unmarshal(raw, &users) != nil {
		return nil
	}
	return users
}

// loadUnchecked is sshKeysUncheckedKey's reasons by username.
func loadUnchecked(ctx context.Context, private privateState) map[string]string {
	raw, diags := private.GetKey(ctx, sshKeysUncheckedKey)
	if diags.HasError() || len(raw) == 0 {
		return nil
	}
	var why map[string]string
	if json.Unmarshal(raw, &why) != nil {
		return nil
	}
	return why
}

func describeUnchecked(why map[string]string) string {
	var parts []string
	for _, user := range slices.Sorted(maps.Keys(why)) {
		parts = append(parts, user+": "+why[user])
	}
	return strings.Join(parts, "; ")
}

// sshKeysBlockedKey holds, from a plan to its apply, why the configured keys
// of some users couldn't be checked: the apply stops on it.
const sshKeysBlockedKey = "ssh_keys_blocked"

// keysNeedRole is how an account gets to read a key's fingerprint. The
// platform applies `credentials-admin` only at platform or project scope
// (k0rdent API v1.3.0-rc.5, its built-in roles). On kind an
// organization-scoped account holding it across the organization was
// refused, and the same account read the key once a policy bound the role
// in the project. The platform passes only compute permissions from an
// organization binding down to its projects, so a role with this permission
// bound across the organization (a custom one, on kind) reads nothing.
const keysNeedRole = "The read needs `credentials.metadata.read` bound in the resource's project. A " +
	"custom role holding only that permission is enough. The built-in `credentials-admin` has it " +
	"too, but can also change and revoke credentials. An organization-scoped account needs the " +
	"role bound in each project it imports into: the platform passes only compute roles from an " +
	"organization down to its projects."

// uncheckedKeysText says which users' configured keys couldn't be checked,
// and why.
func uncheckedKeysText(users []string, why map[string]string) string {
	slices.Sort(users)
	reason := "the last refresh read none: plan with a refresh"
	refused := len(why) == 0
	if known := map[string]string{}; why != nil {
		for _, u := range users {
			if w, ok := why[u]; ok {
				known[u] = w
				refused = refused || strings.Contains(w, "HTTP 403")
			}
		}
		if len(known) > 0 {
			reason = describeUnchecked(known)
		} else {
			refused = true
		}
	}
	advice := ""
	if refused {
		advice = keysNeedRole + " "
	}
	return fmt.Sprintf("The configuration sets keys for %s, and the platform answers keys only as references. "+
		"Checking them takes each key's fingerprint, which the provider couldn't read (%s). Without it a "+
		"changed key would plan no change and the nodes would keep the old one.\n\n%sOr leave those keys "+
		"out of the configuration: `ssh_access` then keeps the platform's users and keys, and nothing is "+
		"checked or sent.", strings.Join(users, ", "), reason, advice)
}

// planUncheckedKeys plans an update for users whose configured keys can't be
// checked, with a warning, and leaves the apply a marker to stop on. The plan
// itself doesn't fail, since `terraform destroy` runs one first and has to
// get past it; it never plans no change either.
func planUncheckedKeys(ctx context.Context, resp *resource.ModifyPlanResponse, users []string, why map[string]string) {
	if len(users) == 0 {
		return
	}
	text := uncheckedKeysText(users, why)
	resp.Diagnostics.AddWarning("The SSH keys can't be checked, so the apply will stop",
		text+"\n\nThe plan shows an update so that the apply runs and stops with this message: no "+
			"change is made. A destroy isn't affected.")
	var planned types.Set
	var all []sshUserModel
	resp.Diagnostics.Append(resp.Plan.GetAttribute(ctx, path.Root("ssh_access"), &planned)...)
	if resp.Diagnostics.HasError() || planned.IsNull() || planned.IsUnknown() {
		return
	}
	resp.Diagnostics.Append(planned.ElementsAs(ctx, &all, false)...)
	for i := range all {
		if slices.Contains(users, all[i].Username.ValueString()) {
			all[i].Credentials = types.ListUnknown(types.StringType)
		}
	}
	marked, d := types.SetValueFrom(ctx, sshUserObjectType(), all)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("ssh_access"), marked)...)
	// Private state holds JSON, so the message goes in as a JSON string.
	if b, err := json.Marshal(text); err == nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, sshKeysBlockedKey, b)...)
	}
}

// refuseUncheckedKeys stops an apply whose plan couldn't check the configured
// keys, before anything is sent, and reports whether it did.
func refuseUncheckedKeys(ctx context.Context, private privateState, diags *diag.Diagnostics) bool {
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return false
	}
	raw, d := private.GetKey(ctx, sshKeysBlockedKey)
	var text string
	if d.HasError() || len(raw) == 0 || json.Unmarshal(raw, &text) != nil || text == "" {
		return false
	}
	diags.AddError("The SSH keys can't be checked", text)
	return true
}

// sameFingerprints compares two users' keys without regard to order: the
// nodes take a set of keys, and the platform's order needn't be the
// configuration's.
func sameFingerprints(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
