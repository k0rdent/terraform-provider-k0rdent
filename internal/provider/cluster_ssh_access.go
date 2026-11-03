package provider

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A cluster's `sshAccess`: the Linux users written onto every node when it is
// provisioned, each with the public keys allowed to log in as that user. It's
// the same shape as an instance group's. `Cluster` doesn't carry it: the
// platform reports it at its own path, `listClusterSshAccess`, which a read
// asks after the cluster itself.
//
// Not every landscape serves that path. Where it's missing, state keeps what
// it held, which is what a create sent. The contract makes the users
// immutable after create ("cannot be changed or removed from a running
// cluster"), so a change is a replacement. That holds only while state's value
// is known to be what the nodes carry: sent by a create, or read from the
// platform. A cluster imported where the path is missing, or created by an
// older version, has no such value. There a changed value is recorded in
// place, sends nothing, and the plan says the nodes keep the users they have.

// sshAccessRecordedKey is the private state key set once the cluster's
// `ssh_access` in state is what its nodes carry: on a create, and on every
// read that got the users from the platform.
const sshAccessRecordedKey = "ssh_access_recorded"

// recordSSHAccess marks the cluster's `ssh_access` in state as what its nodes
// carry. Terraform always hands a create and a read their private state. A
// test that calls one directly may not, and there's nothing to record into.
func recordSSHAccess(ctx context.Context, private privateWriter) diag.Diagnostics {
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return nil
	}
	return private.SetKey(ctx, sshAccessRecordedKey, []byte("true"))
}

// sshAccessRecorded reports whether state's `ssh_access` is what the nodes
// carry.
func sshAccessRecorded(ctx context.Context, private privateState) bool {
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return false
	}
	raw, diags := private.GetKey(ctx, sshAccessRecordedKey)
	return !diags.HasError() && string(raw) == "true"
}

// noSSHUsers is a set with no users in it: null or empty.
func noSSHUsers(v types.Set) bool {
	return v.IsNull() || (!v.IsUnknown() && len(v.Elements()) == 0)
}

// clusterSSHAccessRequiresReplace replaces a cluster whose recorded
// `ssh_access` has changed, since the platform can't change it on a running
// cluster. Null and `[]` both ask for no users, so moving between them
// replaces nothing. Without a record there's nothing to compare against, so
// the change is recorded in place: see warnUnrecordedSSHAccess. Nor does a
// configuration with no users, against a cluster whose last read held users
// the platform had released (teardown_read.go): both say there are none. Nor
// do keys that only moved, as keysOnlyMoved says.
func clusterSSHAccessRequiresReplace(ctx context.Context, req planmodifier.SetRequest, resp *setplanmodifier.RequiresReplaceIfFuncResponse) {
	if noSSHUsers(req.PlanValue) && wasReleased(ctx, req.Private) {
		return
	}
	if keysOnlyMoved(ctx, req, &resp.Diagnostics) {
		return
	}
	resp.RequiresReplace = sshAccessRecorded(ctx, req.Private) &&
		(!noSSHUsers(req.PlanValue) || !noSSHUsers(req.StateValue))
}

// warnUnrecordedSSHAccess says what an in-place `ssh_access` change on an
// unrecorded cluster does, which is nothing to its nodes.
func warnUnrecordedSSHAccess(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || sshAccessRecorded(ctx, req.Private) {
		return
	}
	unrecordedSSHAccessWarning(ctx, req.State, req.Plan, &resp.Diagnostics)
}

// unrecordedSSHAccessWarning is warnUnrecordedSSHAccess once it's known
// that state has no record: a warning when the plan changes `ssh_access`.
func unrecordedSSHAccessWarning(ctx context.Context, state tfsdk.State, plan tfsdk.Plan, diags *diag.Diagnostics) {
	var planned, prior types.Set
	if plan.GetAttribute(ctx, path.Root("ssh_access"), &planned).HasError() ||
		state.GetAttribute(ctx, path.Root("ssh_access"), &prior).HasError() || planned.Equal(prior) ||
		(noSSHUsers(planned) && noSSHUsers(prior)) {
		return
	}
	var id types.String
	_ = state.GetAttribute(ctx, path.Root("id"), &id)
	diags.AddWarning(
		redactDiagnosticText(fmt.Sprintf("`ssh_access` on %s is recorded, not sent", id.ValueString())),
		"The platform takes `ssh_access` only when it creates a cluster. This landscape doesn't report "+
			"a cluster's SSH users, and this cluster was imported, or created by an earlier version of the "+
			"provider, so state has no record of the users its nodes were made with. The apply records the "+
			"configured value and sends nothing, and the nodes keep whatever users they have.\n\nTo put "+
			"these users on the nodes, replace the cluster: `terraform apply -replace` with its address. "+
			"That deletes it and creates it again.")
}

// clusterSSHAccessState is prior state's `ssh_access`, kept when the platform
// says nothing about it. A prior with no type, from a model built by hand, is
// null.
func clusterSSHAccessState(prior types.Set) types.Set {
	if prior.ElementType(context.Background()) == nil {
		return types.SetNull(sshUserObjectType())
	}
	return prior
}

// sshAccessRouteMissing reports whether a status says the landscape doesn't
// serve `listClusterSshAccess`. A 404 is that whoever answered it: without the
// service's envelope a router or region guard had no route for the path, and
// with it, moments after the cluster read 200, the read raced a delete that
// the next refresh will see. Neither says anything about the users. A 405 or a
// 501 is a server that knows the path and not the read.
func sshAccessRouteMissing(status int) bool {
	return status == http.StatusNotFound || status == http.StatusMethodNotAllowed ||
		status == http.StatusNotImplemented
}

// readSSHAccess is the cluster's `ssh_access` after a read, and whether it
// came from the platform. Where the landscape doesn't serve the read, prior
// state is kept without a word. Any other failure keeps it too, with a
// warning, since a refresh shouldn't stop over a field the platform never
// changes.
func (r *clusterResource) readSSHAccess(ctx context.Context, target clusterTarget, id string, prior types.Set) (types.Set, bool, diag.Diagnostics) {
	got, err := r.meta.Client.ListClusterSshAccessWithResponse(ctx, target.region, target.project, id)
	if err == nil && got.JSON200 != nil {
		v, diags := sshAccessState(ctx, got.JSON200, clusterSSHAccessState(prior))
		return v, !diags.HasError(), diags
	}
	// Checked whatever err says: the client reports a body it can't decode
	// as an error, and a router's 404 needn't be the contract's shape.
	if got != nil && sshAccessRouteMissing(got.StatusCode()) {
		return clusterSSHAccessState(prior), false, nil
	}
	var failed, diags diag.Diagnostics
	var httpResp *http.Response
	var body []byte
	if got != nil {
		httpResp, body = got.HTTPResponse, got.Body
	}
	addAPIError(&failed, "read the cluster's SSH access", httpResp, body, err)
	for _, d := range failed.Errors() {
		diags.AddWarning(d.Summary(), d.Detail()+"\n\nState keeps the `ssh_access` it held. A read that "+
			"succeeds replaces it with what the platform reports.")
	}
	return clusterSSHAccessState(prior), false, diags
}

// sshPublicKeysWOAttribute is the users' keys by username, sent on the create
// and never stored, so an import plans no change for them.
func sshPublicKeysWOAttribute() schema.MapAttribute {
	return schema.MapAttribute{
		Optional:    true,
		WriteOnly:   true,
		ElementType: types.ListType{ElemType: types.StringType},
		MarkdownDescription: "Public keys for the users in `ssh_access`, by username, sent on the create " +
			"and never stored. Requires Terraform 1.11 or OpenTofu 1.11 or later. Use it in place of a " +
			"user's `ssh_public_keys` so that an import with the same configuration plans no changes: " +
			"the platform reports only references to the keys, which state holds as " +
			"`ssh_public_key_refs`. A changed key is found by its fingerprint and planned as a change " +
			"to `ssh_access`. After a create the provider checks against the " +
			"keys it sent. After an import it reads each key's fingerprint from the platform, which needs " +
			"`credentials.metadata.read` bound in the project. " +
			"Without it, the plan warns and the apply stops: it never plans no change " +
			"over keys it can't check.",
	}
}

// sentKeys is the keys a create body carried, by username.
func sentKeys(users *[]k0rdent.SshUser) map[string][]string {
	out := map[string][]string{}
	if users == nil {
		return out
	}
	for _, u := range *users {
		if u.SshPublicKeys != nil {
			out[u.Username] = *u.SshPublicKeys
		}
	}
	return out
}

// replaceOnChangedKeys replaces the resource when a configured key differs, by
// fingerprint, from the keys the nodes were made with, in either layout. The
// changed users' references are planned unknown, since Terraform drops a
// replacement whose path planned no change.
func replaceOnChangedKeys(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var configured types.Map
	if req.Config.GetAttribute(ctx, path.Root("ssh_public_keys_wo"), &configured).HasError() {
		return
	}
	keys, unknown, diags := writeOnlyKeys(ctx, configured)
	if diags.HasError() {
		return
	}
	// The marker is a plan's, for its own apply: none carries over.
	if was, d := req.Private.GetKey(ctx, sshKeysBlockedKey); !d.HasError() && len(was) > 0 {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, sshKeysBlockedKey, nil)...)
	}
	held := loadFingerprints(ctx, req.Private)
	text := usersWithConfiguredKeys(ctx, req.Config)
	// Keys state holds as text came from a create, this version's or an
	// earlier one's, and are what the nodes were made with.
	inState := usersWithConfiguredKeys(ctx, req.State)
	// Only users the platform holds keys for, by reference, can be checked;
	// any other is an added user, which replaces.
	referenced := usersWithReferences(ctx, req.State)
	var unchecked []string
	// Keys not known until the apply can't be checked at plan either.
	for user := range referenced {
		if _, ok := held[user]; ok {
			continue
		}
		if ks, set := keys[user]; (set && (ks == nil || len(ks) > 0)) || (unknown && !set) {
			unchecked = append(unchecked, user)
		}
	}
	for user, ks := range text {
		if _, ok := held[user]; !ok && (ks == nil || len(ks) > 0) && len(inState[user]) == 0 && referenced[user] &&
			!slices.Contains(unchecked, user) {
			unchecked = append(unchecked, user)
		}
	}
	if len(unchecked) > 0 {
		planUncheckedKeys(ctx, resp, unchecked, loadUnchecked(ctx, req.Private))
		return
	}
	changed := keysChanged(held, fingerprintsOf(keys))
	// Keys unknown at plan may differ.
	for user := range held {
		if ks, ok := keys[user]; unknown || (ok && ks == nil) {
			changed = append(changed, user)
		}
	}
	// Keys written as text are checked the same way, against the platform's
	// fingerprints where a refresh read them.
	for user, ks := range text {
		if have, ok := held[user]; ok && ks != nil && !sameFingerprints(fingerprintList(ks), have) {
			changed = append(changed, user)
		}
	}
	warnRevokedKeys(&resp.Diagnostics, loadRevoked(ctx, req.Private))
	slices.Sort(changed)
	changed = slices.Compact(changed)
	if len(changed) == 0 {
		return
	}
	var planned types.Set
	var users []sshUserModel
	resp.Diagnostics.Append(resp.Plan.GetAttribute(ctx, path.Root("ssh_access"), &planned)...)
	if resp.Diagnostics.HasError() || planned.IsNull() || planned.IsUnknown() {
		return
	}
	resp.Diagnostics.Append(planned.ElementsAs(ctx, &users, false)...)
	for i := range users {
		if slices.Contains(changed, users[i].Username.ValueString()) {
			users[i].Credentials = types.ListUnknown(types.StringType)
		}
	}
	marked, d := types.SetValueFrom(ctx, sshUserObjectType(), users)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("ssh_access"), marked)...)
	resp.RequiresReplace = append(resp.RequiresReplace, path.Root("ssh_access"))
}

// usersWithConfiguredKeys is the keys `ssh_access` sets as text in a
// configuration or a state, by username, nil for keys not known until the
// apply.
func usersWithConfiguredKeys(ctx context.Context, config interface {
	GetAttribute(context.Context, path.Path, any) diag.Diagnostics
}) map[string][]string {
	var access types.Set
	var users []sshUserModel
	if config.GetAttribute(ctx, path.Root("ssh_access"), &access).HasError() || access.IsNull() ||
		access.IsUnknown() || access.ElementsAs(ctx, &users, false).HasError() {
		return nil
	}
	out := map[string][]string{}
	for _, u := range users {
		if u.Username.IsUnknown() || u.SSHPublicKeys.IsNull() {
			continue
		}
		var ks []string
		if !keysUnknown(u.SSHPublicKeys) && (u.SSHPublicKeys.ElementsAs(ctx, &ks, false).HasError() || len(ks) == 0) {
			continue
		}
		out[u.Username.ValueString()] = ks
	}
	return out
}

// usersWithReferences is the users state holds key references for.
func usersWithReferences(ctx context.Context, state tfsdk.State) map[string]bool {
	var access types.Set
	var users []sshUserModel
	if state.GetAttribute(ctx, path.Root("ssh_access"), &access).HasError() || access.IsNull() ||
		access.IsUnknown() || access.ElementsAs(ctx, &users, false).HasError() {
		return nil
	}
	out := map[string]bool{}
	for _, u := range users {
		if !u.Credentials.IsNull() && !u.Credentials.IsUnknown() && len(u.Credentials.Elements()) > 0 {
			out[u.Username.ValueString()] = true
		}
	}
	return out
}

// keysChanged is the users whose keys differ from the ones held for them. A
// user with none held is replaceOnChangedKeys' error, not a change.
func keysChanged(held, want map[string][]string) []string {
	var changed []string
	for user, fps := range want {
		if have, ok := held[user]; ok && !sameFingerprints(fps, have) {
			changed = append(changed, user)
		}
	}
	slices.Sort(changed)
	return changed
}

// onlyReferencesUnknown reports whether two `ssh_access` values differ only
// in references planned unknown by replaceOnChangedKeys.
func onlyReferencesUnknown(from, to attr.Value) bool {
	f, okFrom := from.(types.Set)
	t, okTo := to.(types.Set)
	if !okFrom || !okTo || f.IsNull() || t.IsNull() || f.IsUnknown() || t.IsUnknown() {
		return from.Equal(to)
	}
	strip := func(s types.Set) (types.Set, bool) {
		var users []sshUserModel
		if s.ElementsAs(context.Background(), &users, false).HasError() {
			return s, false
		}
		for i := range users {
			users[i].Credentials = types.ListNull(types.StringType)
		}
		out, d := types.SetValueFrom(context.Background(), sshUserObjectType(), users)
		return out, !d.HasError()
	}
	fs, ok1 := strip(f)
	ts, ok2 := strip(t)
	return ok1 && ok2 && fs.Equal(ts)
}

// checkConfiguredKeys fails a create's plan on a user with no keys, keys
// written both ways, or write-only keys for a user `ssh_access` lacks. On an
// existing resource they warn, so a plan before a destroy still runs.
func checkConfiguredKeys(ctx context.Context, req resource.ModifyPlanRequest, diags *diag.Diagnostics) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var access types.Set
	var configured types.Map
	if req.Config.GetAttribute(ctx, path.Root("ssh_access"), &access).HasError() ||
		req.Config.GetAttribute(ctx, path.Root("ssh_public_keys_wo"), &configured).HasError() ||
		access.IsUnknown() || configured.IsUnknown() {
		return
	}
	keys, _, d := writeOnlyKeys(ctx, configured)
	var users []sshUserModel
	if d.HasError() || (!access.IsNull() && access.ElementsAs(ctx, &users, false).HasError()) {
		return
	}
	creating := req.State.Raw.IsNull()
	report := diags.AddAttributeWarning
	if creating {
		report = diags.AddAttributeError
	}
	named, allKnown := map[string]bool{}, true
	for _, u := range users {
		if u.Username.IsUnknown() {
			allKnown = false
			continue
		}
		name := u.Username.ValueString()
		if named[name] {
			report(path.Root("ssh_access"), "An SSH user is written twice",
				fmt.Sprintf("`ssh_access` names %q more than once. Write each user once.", name))
		}
		named[name] = true
		wo, written := keys[name]
		// Unknown at plan counts as given.
		inline := u.SSHPublicKeys.IsUnknown() || len(u.SSHPublicKeys.Elements()) > 0
		refs := u.Credentials.IsUnknown() || len(u.Credentials.Elements()) > 0
		switch {
		case creating && !inline && !refs && (!written || (wo != nil && len(wo) == 0)):
			report(path.Root("ssh_access"), "An SSH user has no keys",
				fmt.Sprintf("`ssh_access` names %q with no keys, and the platform refuses a user without one. "+
					"Write the user's keys in `ssh_public_keys_wo`, as `%s = [...]`, or in the user's "+
					"`ssh_public_keys`, or give `ssh_public_key_refs`.", name, name))
		case inline && written:
			report(path.Root("ssh_public_keys_wo"), "An SSH user's keys are written twice",
				fmt.Sprintf("%q has keys in both its `ssh_public_keys` and `ssh_public_keys_wo`. A create "+
					"sends only the write-only ones. Keep one of the two.", name))
		case (inline || written) && refs:
			report(path.Root("ssh_access"), "An SSH user has both keys and references",
				fmt.Sprintf("%q has keys and `ssh_public_key_refs`. A create sends one or the other, and "+
					"the platform makes new references from keys. Keep the keys, or the references.", name))
		}
	}
	if !allKnown {
		return
	}
	for _, user := range slices.Sorted(maps.Keys(keys)) {
		if !named[user] {
			report(path.Root("ssh_public_keys_wo"), "Keys for a user `ssh_access` doesn't name",
				fmt.Sprintf("`ssh_public_keys_wo` has keys for %q, but `ssh_access` has no user of that name, so "+
					"the keys would go nowhere. Add the user to `ssh_access`, or correct the name.", user))
		}
	}
}

// planNoUsersWhenLeftOut plans `ssh_access` null on a create that leaves it
// out, which is what the platform reports back.
func planNoUsersWhenLeftOut(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if !req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var configured types.Set
	if req.Config.GetAttribute(ctx, path.Root("ssh_access"), &configured).HasError() || !configured.IsNull() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("ssh_access"), types.SetNull(sshUserObjectType()))...)
}
