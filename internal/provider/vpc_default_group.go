package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// defaultGroupState is what the provider knows about whether a VPC has a
// default security group to fall back to.
type defaultGroupState int

const (
	// defaultGroupUnknown is a read that failed or couldn't tell.
	defaultGroupUnknown defaultGroupState = iota
	defaultGroupPresent
	// defaultGroupMissing is a VPC the API v1.3.0-rc.4 made no default group
	// for: one with no subnet prefixes, or more than 16. It refuses an empty
	// binding with 409, so its groups can't be taken off.
	defaultGroupMissing
)

// noDefaultGroupWhy says which VPCs have no default group, for the texts
// that name one.
const noDefaultGroupWhy = "From the k0rdent API v1.3.0-rc.4, a VPC with no subnet prefixes, or " +
	"more than 16, has no default security group. The platform won't leave a VPC with no group, " +
	"so it refuses to take that VPC's groups off."

// vpcDefaultGroup reads the VPC, then looks for its default group.
func vpcDefaultGroup(ctx context.Context, meta *Meta, target clusterTarget, vpc string) defaultGroupState {
	// A plan before the provider is configured has no client.
	if meta == nil || meta.Client == nil {
		return defaultGroupUnknown
	}
	got, err := meta.Client.GetVPCWithResponse(ctx, target.region, target.project, vpc)
	if err != nil || got.JSON200 == nil {
		return defaultGroupUnknown
	}
	return defaultGroupOf(ctx, meta, target, *got.JSON200)
}

// defaultGroupOf looks for v's default group: the group whose ownerKind is
// `vpc` and whose ownerId is v's uid. A binding can replace the default, so
// the VPC's own list doesn't say whether one exists. The security group list
// has no owner filter, so this reads all of the project's groups.
func defaultGroupOf(ctx context.Context, meta *Meta, target clusterTarget, v k0rdent.VPC) defaultGroupState {
	uid := uidOf(v.Uid)
	if uid == "" {
		return defaultGroupUnknown
	}
	var diags diag.Diagnostics
	groups, ok := listSecurityGroups(ctx, meta, target, k0rdent.ListSecurityGroupsParams{}, &diags)
	if !ok {
		return defaultGroupUnknown
	}
	for _, g := range groups {
		if g.OwnerKind != nil && *g.OwnerKind == k0rdent.SecurityGroupOwnerKindVpc && uidOf(g.OwnerId) == uid {
			return defaultGroupPresent
		}
	}
	return defaultGroupMissing
}

// refusedForNoDefault reports whether a write of [] was refused because the
// VPC has no default group. The reason, CONFLICT_IN_USE, is shared with other
// refusals, so the message is what tells this one apart.
func refusedForNoDefault(resp *http.Response, body []byte) bool {
	var apiErr *k0rdent.APIError
	return resp != nil && resp.StatusCode == http.StatusConflict &&
		errors.As(k0rdent.CheckResponse(resp, body), &apiErr) &&
		strings.Contains(apiErr.Message, "no default security group to fall back to")
}

// leftOnVPC records that a destroy in this run left groups bound to a VPC
// with no default group, so a delete of one of those groups later in the run
// knows the VPC holds it until the VPC goes.
func (l *bindingLedger) leftOnVPC(target clusterTarget, vpc string, groups []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.left == nil {
		l.left = map[string][]string{}
	}
	for _, g := range groups {
		key := bindingKey(target, g)
		if !slices.Contains(l.left[key], vpc) {
			l.left[key] = append(l.left[key], vpc)
		}
	}
}

// leftHolding is the VPCs a destroy in this run left holding the group.
func (l *bindingLedger) leftHolding(target clusterTarget, group string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.left[bindingKey(target, group)])
}

// warnLeftBound is the warning a destroy gives when it leaves the groups on
// a VPC with no default group.
func warnLeftBound(diags *diag.Diagnostics, vpc string, groups []string) {
	diags.AddWarning(
		"Left the security groups on a VPC with no default group",
		redactDiagnosticText(fmt.Sprintf("%q has no default security group to fall back to. %s So %s "+
			"stay bound to %q and keep filtering it until the VPC is deleted with its cluster or "+
			"instance group, and this resource was only dropped from state. A delete of one of "+
			"those groups in this run waits for the VPC to go.",
			vpc, noDefaultGroupWhy, groupList(groups), vpc)),
	)
}

// releasedByLeftVPCs is send, sent again while the platform refuses it only
// because VPCs that a binding destroy in this run left the group on still
// hold it. Such a VPC goes when its owner finishes deleting, usually in the
// same destroy, and Terraform doesn't order the two deletes. Anything else
// holding the group ends the wait and gets the refusal as usual.
func releasedByLeftVPCs(meta *Meta, target clusterTarget, group string, send func(context.Context) (*http.Response, []byte, error)) func(context.Context) (*http.Response, []byte, error) {
	return func(ctx context.Context) (*http.Response, []byte, error) {
		var resp *http.Response
		var body []byte
		var sendErr error
		left := meta.vpcBindings.leftHolding(target, group)
		what := fmt.Sprintf("the teardown of the cluster or instance group that owns the VPC %s, "+
			"which holds the group until it's deleted because it has no default group to fall back to",
			strings.Join(left, ", "))
		waited := false
		waitErr := waitFor(ctx, meta.Wait, what, func(ctx context.Context) (bool, error) {
			resp, body, sendErr = send(ctx)
			// Anything but a refusal from those VPCs alone goes back to the
			// caller, a failed send included, which reports it as usual.
			refusedByLeft := sendErr == nil && resp != nil && resp.StatusCode == http.StatusConflict &&
				heldOnlyBy(ctx, meta, target, group, left)
			if !refusedByLeft {
				return true, nil
			}
			if !waited {
				waited = true
				tflog.Info(ctx, "the security group is held by a VPC this destroy left it on, so the "+
					"delete is sent again once that VPC goes", map[string]any{"group": group, "vpcs": left})
			}
			return false, nil
		})
		if waitErr != nil {
			return nil, nil, waitErr
		}
		return resp, body, sendErr
	}
}

// heldOnlyBy reports whether every live holder of the group is one of vpcs.
// An empty list counts: a VPC tearing down holds the group but isn't listed.
func heldOnlyBy(ctx context.Context, meta *Meta, target clusterTarget, group string, vpcs []string) bool {
	if len(vpcs) == 0 {
		return false
	}
	got, err := meta.Client.ListSecurityGroupAttachmentsWithResponse(ctx, target.region, target.project, group)
	if err != nil || got.JSON200 == nil {
		return false
	}
	for _, a := range got.JSON200.Attachments {
		if a.Kind != "vpc" || !slices.Contains(vpcs, a.Id) {
			return false
		}
	}
	return true
}
