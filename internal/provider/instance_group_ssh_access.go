package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// An instance group's `sshAccess`: the Linux users written into every host's
// cloud-init at provisioning time, each with the public keys allowed to log
// in as that user.
//
// A group created without it can fail with INVALID_CONFIGURATION. The contract
// calls the field optional, so the provider does too, and
// missingSSHAccessAdvice names the likely cause beside the platform's answer.
//
// The platform returns entries sorted by username, each user's keys in the
// order they were sent.
// So the entries are a set, and the keys inside one are a list.

var sshUsernamePattern = regexp.MustCompile(k0rdent.SshUserUsernamePattern)

// sshKeyPattern is one line with no whitespace at either end. The platform
// trims each key when it stores it and again when it reads it back
// (on a k0rdent API build from main), so a key sent
// with the trailing newline that `file()` keeps from a .pub file reads back
// different. The create would fail as an inconsistent result after the hosts
// were provisioned, and the next plan would replace the group. Refusing it at
// plan time costs a `trimspace()`.
var sshKeyPattern = regexp.MustCompile(`^\S(.*\S)?$`)

type sshUserModel struct {
	Username      types.String `tfsdk:"username"`
	SSHPublicKeys types.List   `tfsdk:"ssh_public_keys"`
	Credentials   types.List   `tfsdk:"ssh_public_key_refs"`
	Sudo          types.String `tfsdk:"sudo"`
	DisplayName   types.String `tfsdk:"display_name"`
}

func sshUserObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"username":            types.StringType,
		"ssh_public_keys":     types.ListType{ElemType: types.StringType},
		"ssh_public_key_refs": types.ListType{ElemType: types.StringType},
		"sudo":                types.StringType,
		"display_name":        types.StringType,
	}}
}

// Descriptions shared by the resource and the data source. The pattern and
// the bounds are the client's, the ones the validators check.
var (
	sshUsernameDescription = "Linux username to provision on every host. Must match " +
		"`" + k0rdent.SshUserUsernamePattern + "`."
	sshPublicKeysDescription = fmt.Sprintf("Public keys, each one `authorized_keys` line, allowed to log in as this "+
		"user. From %d to %d. The platform accepts `ssh-ed25519`, `ecdsa-sha2-nistp256/384/521`, "+
		"`ssh-rsa` of at least 2048 bits, and the `sk-` security-key variants. Kept in the order written. "+
		"A key must not start or end with whitespace, since the platform trims it: read one from a file "+
		"with `trimspace(file(...))`, because `file()` keeps the trailing newline.",
		k0rdent.SshUserSshPublicKeysMinItems, k0rdent.SshUserSshPublicKeysMaxItems)
)

const (
	sshCredentialsDescription = "References to the `ssh_public_key` credentials holding this user's keys, as " +
		"the platform reports them. Keys given as text are registered as credentials when the cluster or " +
		"group is created, and read back here. Give references instead of keys to use credentials that " +
		"already exist; an import fills them in."
	sshSudoDescription        = "A sudo fragment granting this user privileges, such as `ALL=(ALL) NOPASSWD:ALL`."
	sshDisplayNameDescription = "Human-friendly name for this entry."
)

// missingSSHAccessAdvice names the likely cause when a group created with no
// `ssh_access` users, left out or `[]`, settles `failed` with reason
// INVALID_CONFIGURATION, since the platform's answer doesn't name it. last is
// the final body the wait read, and the reason is taken from it, not from any
// message.
// Any other failure, or a group that had users, gets no advice.
func missingSSHAccessAdvice(access types.Set, last *k0rdent.InstanceGroup) string {
	if last == nil || last.State == nil || *last.State != k0rdent.InstanceGroupStateFailed ||
		last.StateReason == nil || *last.StateReason != k0rdent.StateReasonINVALIDCONFIGURATION {
		return ""
	}
	if access.IsUnknown() || (!access.IsNull() && len(access.Elements()) > 0) {
		return ""
	}
	return "This group has no `ssh_access`, a common cause of this failure. Add a user and key, " +
		"then apply again, which replaces the group."
}

// sshAccessRequiresReplace makes every change to `ssh_access` a replacement
// but two, which record the change in place and send nothing. One is a
// configuration with no users, planned against a group whose last read held
// users the platform had released (teardown_read.go): the platform and the
// configuration agree there are none. The other is keys written for users an
// import read with none, or keys moved to or from `ssh_public_keys_wo`: see
// keysOnlyMoved.
func sshAccessRequiresReplace(ctx context.Context, req planmodifier.SetRequest, resp *setplanmodifier.RequiresReplaceIfFuncResponse) {
	if keysOnlyMoved(ctx, req, &resp.Diagnostics) {
		return
	}
	none := req.ConfigValue.IsNull() || (!req.ConfigValue.IsUnknown() && len(req.ConfigValue.Elements()) == 0)
	resp.RequiresReplace = !none || !wasReleased(ctx, req.Private)
}

// keysUnknown reports whether a user's keys aren't all known at plan: the
// list, or any key in it, such as a key another resource makes in the same
// apply. Either way there's nothing to compare yet.
func keysUnknown(keys types.List) bool {
	if keys.IsUnknown() {
		return true
	}
	for _, k := range keys.Elements() {
		if k.IsUnknown() {
			return true
		}
	}
	return false
}

// keysOnlyMoved reports whether only where the keys are written changed: keys
// written over none after an import, moved between `ssh_public_keys` and
// `ssh_public_keys_wo`, or left out. Keys are compared by fingerprint, and
// ones with nothing to compare against are recorded with a warning.
func keysOnlyMoved(ctx context.Context, req planmodifier.SetRequest, diags *diag.Diagnostics) bool {
	writeOnly, unknown, d := configuredWriteOnlyKeys(ctx, req.Config)
	if d.HasError() || unknown {
		return false
	}
	return keysMoved(ctx, req.StateValue, req.PlanValue, writeOnly, loadFingerprints(ctx, req.Private), diags)
}

// keysMoved is keysOnlyMoved given the write-only keys configured and the
// fingerprints of the keys the nodes were made with. Keys with none to check
// against are recorded in place here, and so are the references of a user
// with none, which only planUncheckedKeys plans unknown: the update it plans
// stops at the apply, so nothing is ever recorded unchecked, and it's no
// replacement.
func keysMoved(ctx context.Context, state, plan types.Set, writeOnly, madeWith map[string][]string, diags *diag.Diagnostics) bool {
	if state.IsNull() || state.IsUnknown() || plan.IsNull() || plan.IsUnknown() || state.Equal(plan) {
		return false
	}
	var held, planned []sshUserModel
	if state.ElementsAs(ctx, &held, false).HasError() || plan.ElementsAs(ctx, &planned, false).HasError() ||
		len(held) != len(planned) {
		return false
	}
	byName := make(map[string]sshUserModel, len(held))
	for _, u := range held {
		byName[u.Username.ValueString()] = u
	}
	for _, p := range planned {
		name := p.Username.ValueString()
		h, ok := byName[name]
		_, checked := madeWith[name]
		blocked := !checked && p.Credentials.IsUnknown()
		if !ok || !h.Sudo.Equal(p.Sudo) || !h.DisplayName.Equal(p.DisplayName) ||
			(!blocked && (p.Credentials.IsUnknown() || !h.Credentials.Equal(p.Credentials))) {
			return false
		}
		// Each held user once, so a username written twice can't stand in
		// for a user the plan drops.
		delete(byName, name)
		if h.SSHPublicKeys.Equal(p.SSHPublicKeys) {
			continue
		}
		// What the nodes were made with: the fingerprints held from the create
		// or the import, or the keys state holds. With neither, nothing can be
		// compared, and planUncheckedKeys plans the update the apply stops,
		// whether the configured key is known yet or not.
		have, known := madeWith[name]
		if !known && !keysUnknown(h.SSHPublicKeys) && len(h.SSHPublicKeys.Elements()) > 0 {
			var keys []string
			diags.Append(h.SSHPublicKeys.ElementsAs(ctx, &keys, false)...)
			have, known = fingerprintList(keys), true
		}
		if keysUnknown(p.SSHPublicKeys) || keysUnknown(h.SSHPublicKeys) {
			if !known {
				continue
			}
			return false // not known until the apply, so it can't be the key held
		}
		want, ok := writeOnly[name]
		if ok && want == nil {
			return false // not known until the apply
		}
		if !ok {
			diags.Append(p.SSHPublicKeys.ElementsAs(ctx, &want, false)...)
		}
		if len(want) > 0 && known && !sameFingerprints(fingerprintList(want), have) {
			return false
		}
	}
	return true
}

// sshAccessRequest turns the configured set into the create body's list. Nil
// when the configuration has none, so the field is left out of the request.
func sshAccessRequest(ctx context.Context, v types.Set, writeOnly map[string][]string) (*[]k0rdent.SshUser, diag.Diagnostics) {
	var diags diag.Diagnostics
	if v.IsNull() || v.IsUnknown() {
		return nil, diags
	}
	var users []sshUserModel
	diags.Append(v.ElementsAs(ctx, &users, false)...)
	if diags.HasError() {
		return nil, diags
	}
	out := make([]k0rdent.SshUser, 0, len(users))
	for _, u := range users {
		user := k0rdent.SshUser{
			Username:    u.Username.ValueString(),
			Sudo:        optionalString(u.Sudo),
			DisplayName: optionalString(u.DisplayName),
		}
		switch keys, ok := writeOnly[u.Username.ValueString()]; {
		case ok:
			user.SshPublicKeys = &keys
		case !u.SSHPublicKeys.IsNull() && !u.SSHPublicKeys.IsUnknown():
			keys := []string{}
			diags.Append(u.SSHPublicKeys.ElementsAs(ctx, &keys, false)...)
			user.SshPublicKeys = &keys
		case !u.Credentials.IsNull() && !u.Credentials.IsUnknown():
			refs := []string{}
			diags.Append(u.Credentials.ElementsAs(ctx, &refs, false)...)
			user.SshPublicKeyCredentials = &refs
		}
		out = append(out, user)
	}
	return &out, diags
}

// sshAccessValue maps the platform's echo to a set. An absent or empty echo
// is null.
func sshAccessValue(ctx context.Context, echo *[]k0rdent.SshUser) (types.Set, diag.Diagnostics) {
	return sshAccessValueOver(ctx, echo, nil)
}

// sshAccessValueOver is sshAccessValue with each user's prior keys, by
// username, for a user echoed without keys. A user always has at least one
// key, so an echo with none says nothing about them: an API that registers
// the keys as SSH key credentials answers with references to those instead
// (the k0rdent API v1.3.0-rc.1 on clusters). Taking that as "no keys" would plan a
// replacement on every refresh.
func sshAccessValueOver(ctx context.Context, echo *[]k0rdent.SshUser, priorKeys map[string]types.List) (types.Set, diag.Diagnostics) {
	var diags diag.Diagnostics
	if echo == nil || len(*echo) == 0 {
		return types.SetNull(sshUserObjectType()), diags
	}
	users := make([]sshUserModel, 0, len(*echo))
	for _, u := range *echo {
		echoed := []string{}
		if u.SshPublicKeys != nil {
			echoed = *u.SshPublicKeys
		}
		keys, d := types.ListValueFrom(ctx, types.StringType, echoed)
		diags.Append(d...)
		if len(echoed) == 0 {
			keys = types.ListNull(types.StringType)
		}
		if prior, ok := priorKeys[u.Username]; ok && len(echoed) == 0 {
			keys = prior
		}
		refs := types.ListNull(types.StringType)
		if u.SshPublicKeyCredentials != nil && len(*u.SshPublicKeyCredentials) > 0 {
			refs, d = types.ListValueFrom(ctx, types.StringType, *u.SshPublicKeyCredentials)
			diags.Append(d...)
		}
		users = append(users, sshUserModel{
			Username:      types.StringValue(u.Username),
			SSHPublicKeys: keys,
			Credentials:   refs,
			Sudo:          stringOrNull(u.Sudo),
			DisplayName:   stringOrNull(u.DisplayName),
		})
	}
	set, d := types.SetValueFrom(ctx, sshUserObjectType(), users)
	diags.Append(d...)
	return set, diags
}

// sshAccessState is the resource's reading of the echo against what state
// held before.
//
//   - An echo with entries is the value. A set compares without order, so the
//     platform sorting the entries is no change.
//   - No echo at all, with a known value in state, keeps that value. An API
//     that does not echo the field says nothing about it, and treating that as
//     "removed" would plan a replacement of every group on each refresh.
//   - An empty echo, or none, with nothing in state, is null. An empty echo
//     keeps a state that was itself an empty set, so `ssh_access = []` plans
//     clean.
//   - A user echoed without keys keeps the keys state held for that username.
func sshAccessState(ctx context.Context, echo *[]k0rdent.SshUser, prior types.Set) (types.Set, diag.Diagnostics) {
	priorKnown := !prior.IsNull() && !prior.IsUnknown()
	switch {
	case echo == nil && priorKnown:
		return prior, nil
	case echo != nil && len(*echo) == 0 && priorKnown && len(prior.Elements()) == 0:
		return prior, nil
	}
	var priorKeys map[string]types.List
	if priorKnown {
		var users []sshUserModel
		if d := prior.ElementsAs(ctx, &users, false); !d.HasError() {
			priorKeys = make(map[string]types.List, len(users))
			for _, u := range users {
				if !u.SSHPublicKeys.IsNull() && !u.SSHPublicKeys.IsUnknown() {
					priorKeys[u.Username.ValueString()] = u.SSHPublicKeys
				}
			}
		}
	}
	return sshAccessValueOver(ctx, echo, priorKeys)
}

// sshUserNestedObject is one `ssh_access` entry, the same on an instance
// group and a cluster: a Linux user and the public keys allowed to log in as it.
func sshUserNestedObject() schema.NestedAttributeObject {
	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"username": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: sshUsernameDescription,
				Validators: []validator.String{stringvalidator.RegexMatches(sshUsernamePattern,
					"must start with a lowercase letter or an underscore, then up to 31 lowercase "+
						"letters, digits, underscores or hyphens")},
			},
			"ssh_public_keys": schema.ListAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: sshPublicKeysDescription,
				Validators: []validator.List{
					describedList{listvalidator.SizeBetween(k0rdent.SshUserSshPublicKeysMinItems, k0rdent.SshUserSshPublicKeysMaxItems),
						fmt.Sprintf("must hold from %d to %d keys",
							k0rdent.SshUserSshPublicKeysMinItems, k0rdent.SshUserSshPublicKeysMaxItems)},
					describedList{listvalidator.ValueStringsAre(
						stringvalidator.RegexMatches(sshKeyPattern,
							"must be one line with no whitespace at either end, so read a key from a file "+
								"with trimspace(file(...))"),
						stringvalidator.LengthAtMost(k0rdent.SshUserSshPublicKeysItemMaxLength),
					), fmt.Sprintf("each key must be one line with no whitespace at either end, and at "+
						"most %d characters long. Read one from a file with `trimspace(file(...))`",
						k0rdent.SshUserSshPublicKeysItemMaxLength)},
				},
			},
			"ssh_public_key_refs": schema.ListAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: sshCredentialsDescription,
				Validators: []validator.List{
					describedList{listvalidator.SizeBetween(k0rdent.SshUserSshPublicKeyCredentialsMinItems, k0rdent.SshUserSshPublicKeyCredentialsMaxItems),
						fmt.Sprintf("must hold from %d to %d references",
							k0rdent.SshUserSshPublicKeyCredentialsMinItems, k0rdent.SshUserSshPublicKeyCredentialsMaxItems)},
					describedList{listvalidator.ValueStringsAre(stringvalidator.RegexMatches(
						regexp.MustCompile(k0rdent.SshUserSshPublicKeyCredentialsItemPattern),
						"must be the self-link of an ssh_public_key credential, as an import reads it")),
						"each reference must be the self-link of an `ssh_public_key` credential in this " +
							"project, as an import reads it"},
				},
			},
			"sudo": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: sshSudoDescription,
				Validators:          []validator.String{stringvalidator.LengthAtMost(k0rdent.SshUserSudoMaxLength)},
			},
			"display_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: sshDisplayNameDescription,
				Validators:          []validator.String{stringvalidator.LengthAtMost(k0rdent.SshUserDisplayNameMaxLength)},
			},
		},
	}
}

// sshFingerprint is a key's SHA-256 fingerprint as `ssh-keygen -l` prints it,
// or "" for a line that isn't a key.
func sshFingerprint(line string) string {
	f := strings.Fields(line)
	if len(f) < 2 {
		return ""
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// sshKeyFingerprintsKey holds, by username, the fingerprints of the keys the
// nodes were made with.
const sshKeyFingerprintsKey = "ssh_key_fingerprints"

func fingerprintsOf(keys map[string][]string) map[string][]string {
	out := make(map[string][]string, len(keys))
	for user, ks := range keys {
		if ks != nil {
			out[user] = fingerprintList(ks)
		}
	}
	return out
}

func fingerprintList(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = sshFingerprint(k)
	}
	return out
}

func saveFingerprints(ctx context.Context, private privateWriter, fps map[string][]string) diag.Diagnostics {
	// As recordSSHAccess: a test that calls a create directly may hand it none.
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return nil
	}
	b, err := json.Marshal(fps)
	if err != nil {
		return nil
	}
	return private.SetKey(ctx, sshKeyFingerprintsKey, b)
}

func loadFingerprints(ctx context.Context, private privateState) map[string][]string {
	raw, diags := private.GetKey(ctx, sshKeyFingerprintsKey)
	if diags.HasError() || len(raw) == 0 {
		return nil
	}
	var fps map[string][]string
	if json.Unmarshal(raw, &fps) != nil {
		return nil
	}
	return fps
}

// writeOnlyKeys reads `ssh_public_keys_wo`. A user whose keys aren't known yet
// maps to nil, and unknown says the whole map isn't.
func writeOnlyKeys(ctx context.Context, v types.Map) (keys map[string][]string, unknown bool, diags diag.Diagnostics) {
	if v.IsNull() || v.IsUnknown() {
		return nil, v.IsUnknown(), nil
	}
	keys = map[string][]string{}
	for user, e := range v.Elements() {
		list, ok := e.(types.List)
		switch {
		case !ok || list.IsNull():
		case list.IsUnknown() || slices.ContainsFunc(list.Elements(), attr.Value.IsUnknown):
			keys[user] = nil
		default:
			ks := []string{}
			diags.Append(list.ElementsAs(ctx, &ks, false)...)
			keys[user] = ks
		}
	}
	return keys, false, diags
}

// sshAccessWithoutUnknownCredentials makes any reference list still unknown
// after an apply null, for a landscape that doesn't report them.
func sshAccessWithoutUnknownCredentials(ctx context.Context, v types.Set, diags *diag.Diagnostics) types.Set {
	if v.IsNull() || v.IsUnknown() {
		return v
	}
	var users []sshUserModel
	diags.Append(v.ElementsAs(ctx, &users, false)...)
	for i := range users {
		if users[i].Credentials.IsUnknown() {
			users[i].Credentials = types.ListNull(types.StringType)
		}
	}
	out, d := types.SetValueFrom(ctx, sshUserObjectType(), users)
	diags.Append(d...)
	return out
}

// configuredWriteOnlyKeys is writeOnlyKeys from a configuration, if there is one.
func configuredWriteOnlyKeys(ctx context.Context, config tfsdk.Config) (map[string][]string, bool, diag.Diagnostics) {
	if config.Raw.IsNull() || !config.Raw.IsKnown() {
		return nil, false, nil
	}
	var v types.Map
	if diags := config.GetAttribute(ctx, path.Root("ssh_public_keys_wo"), &v); diags.HasError() {
		return nil, false, diags
	}
	return writeOnlyKeys(ctx, v)
}

// refsByUsername keeps each user's keys and references from state, by
// username, where the configuration leaves them out. So moving keys to
// `ssh_public_keys_wo` plans no change. UseStateForUnknown matches set
// elements by position, which swaps them. On a create there's nothing to keep.
type refsByUsername struct{}

func (refsByUsername) Description(context.Context) string {
	return "keys and references kept from state, by username"
}

func (m refsByUsername) MarkdownDescription(ctx context.Context) string { return m.Description(ctx) }

func (refsByUsername) PlanModifySet(ctx context.Context, req planmodifier.SetRequest, resp *planmodifier.SetResponse) {
	plan, state := resp.PlanValue, req.StateValue
	if plan.IsNull() || plan.IsUnknown() || state.IsUnknown() {
		return
	}
	var planned, held []sshUserModel
	if plan.ElementsAs(ctx, &planned, false).HasError() ||
		(!state.IsNull() && state.ElementsAs(ctx, &held, false).HasError()) {
		return
	}
	byName := make(map[string]sshUserModel, len(held))
	for _, u := range held {
		byName[u.Username.ValueString()] = u
	}
	// Only what the configuration leaves out: a value configured but unknown
	// at plan stays unknown.
	var configured []sshUserModel
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() ||
		req.ConfigValue.ElementsAs(ctx, &configured, false).HasError() {
		return
	}
	leftOut := make(map[string]sshUserModel, len(configured))
	for _, u := range configured {
		if !u.Username.IsUnknown() {
			leftOut[u.Username.ValueString()] = u
		}
	}
	for i, u := range planned {
		if u.Username.IsUnknown() {
			continue
		}
		name := u.Username.ValueString()
		h, ok := byName[name]
		c, inConfig := leftOut[name]
		if !inConfig {
			continue
		}
		if u.Credentials.IsUnknown() && c.Credentials.IsNull() && ok {
			planned[i].Credentials = h.Credentials
		}
		if u.SSHPublicKeys.IsUnknown() && c.SSHPublicKeys.IsNull() {
			planned[i].SSHPublicKeys = types.ListNull(types.StringType)
			if ok {
				planned[i].SSHPublicKeys = h.SSHPublicKeys
			}
		}
	}
	out, d := types.SetValueFrom(ctx, sshUserObjectType(), planned)
	resp.Diagnostics.Append(d...)
	if !d.HasError() {
		resp.PlanValue = out
	}
}

// holdKeysInState holds fingerprints of keys state has as text, for users with
// none held, so a later move to `ssh_public_keys_wo` can still be checked.
func holdKeysInState(ctx context.Context, private interface {
	privateState
	privateWriter
}, access types.Set) diag.Diagnostics {
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) ||
		access.IsNull() || access.IsUnknown() {
		return nil
	}
	held := loadFingerprints(ctx, private)
	if held == nil {
		held = map[string][]string{}
	}
	var users []sshUserModel
	if access.ElementsAs(ctx, &users, false).HasError() {
		return nil
	}
	added := false
	for _, u := range users {
		name := u.Username.ValueString()
		var keys []string
		if _, ok := held[name]; ok || u.SSHPublicKeys.IsUnknown() || u.SSHPublicKeys.ElementsAs(ctx, &keys, false).HasError() || len(keys) == 0 {
			continue
		}
		held[name] = fingerprintList(keys)
		added = true
	}
	if !added {
		return nil
	}
	return saveFingerprints(ctx, private, held)
}
