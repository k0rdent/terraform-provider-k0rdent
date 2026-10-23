package provider

import (
	"context"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
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
	Sudo          types.String `tfsdk:"sudo"`
	DisplayName   types.String `tfsdk:"display_name"`
}

func sshUserObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"username":        types.StringType,
		"ssh_public_keys": types.ListType{ElemType: types.StringType},
		"sudo":            types.StringType,
		"display_name":    types.StringType,
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
// but one: a configuration with no users, planned against a group whose last
// read held users the platform had released (teardown_read.go). The platform
// and the configuration agree there are none, so the change records that in
// place and sends nothing.
func sshAccessRequiresReplace(ctx context.Context, req planmodifier.SetRequest, resp *setplanmodifier.RequiresReplaceIfFuncResponse) {
	none := req.ConfigValue.IsNull() || (!req.ConfigValue.IsUnknown() && len(req.ConfigValue.Elements()) == 0)
	resp.RequiresReplace = !none || !wasReleased(ctx, req.Private)
}

// sshAccessRequest turns the configured set into the create body's list. Nil
// when the configuration has none, so the field is left out of the request.
func sshAccessRequest(ctx context.Context, v types.Set) (*[]k0rdent.SshUser, diag.Diagnostics) {
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
		keys := []string{}
		diags.Append(u.SSHPublicKeys.ElementsAs(ctx, &keys, false)...)
		out = append(out, k0rdent.SshUser{
			Username:      u.Username.ValueString(),
			SshPublicKeys: &keys,
			Sudo:          optionalString(u.Sudo),
			DisplayName:   optionalString(u.DisplayName),
		})
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
		if prior, ok := priorKeys[u.Username]; ok && len(echoed) == 0 {
			keys = prior
		}
		users = append(users, sshUserModel{
			Username:      types.StringValue(u.Username),
			SSHPublicKeys: keys,
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
				Required:            true,
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
