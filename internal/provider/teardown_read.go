package provider

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// What a read keeps while the platform tears a resource down.
//
// Some kinds stop reporting part of what their create sent once a delete
// starts. The platform releases it in the same transaction that moves the
// resource to `deleting`, so every read after that answers it empty: a
// group's SSH users and a cluster's. A teardown that fails settles the
// resource `failed` with them still gone, since nothing puts them back. A
// cluster's node pools stay until the delete settles `deleted` (API v1.3.0),
// so they aren't held here.
//
// Written into state, that empty answer is a difference from the
// configuration. On an immutable attribute it's a replacement the guard
// refuses, which stopped both `terraform plan` and `terraform destroy` on a
// group with SSH access while its delete was in flight. So a read of a
// resource in one of its kind's released states keeps the prior value of an
// attribute the platform answered blank, the way a provider keeps a value a
// resource stops reporting while it's torn down. A non-blank answer is still
// the answer, and a resource in any other state reads as it always did, so
// real drift on a live resource still shows.
//
// `failed` is held too, so a second destroy of a group whose first teardown
// failed gets past its refresh. A resource left `failed` that way may be kept
// on purpose, though, and then the held value hides that the platform no
// longer has it, so a read in `failed` that holds a value also warns. A
// configuration that drops the attribute agrees with the platform, so the
// read leaves a mark in private state (releasedKey) and the attribute's
// replacement modifier lets that change through in place.
//
// The other kinds were checked too. A network, subnet, VPC peering, security
// group or project reads every field back unchanged in `deleting`, and nothing in the file
// store or security group deletes was found clearing a field they echo, so
// none of them declares a released state. A `creating` read echoes what was
// sent on every kind, so it isn't a released state. An
// `updating` read wasn't captured, and the contract doesn't say an update
// blanks anything.

// releasedInputs reports whether a body read in this state may answer blank
// for what the create sent, because a delete has released it.
func (k stateKind[T]) releasedInputs(body T) bool {
	state, ok := k.state(body)
	return ok && slices.Contains(k.released, state)
}

// heldThroughTeardown is the read's value for one attribute, or prior's when
// the platform released the attribute (released), answered it blank (blank),
// and state holds a value to keep. held says which. An import has no prior
// value, so it takes the read. A prior the read already equals, such as
// `ssh_access = []`, holds nothing: there was nothing to release.
func heldThroughTeardown[V attr.Value](released, blank bool, read, prior V) (value V, held bool) {
	if released && blank && !prior.IsNull() && !prior.IsUnknown() && !prior.Equal(read) {
		return prior, true
	}
	return read, false
}

// releasedKey is the private state key a read sets when it held a value the
// platform answered blank, and clears otherwise.
const releasedKey = "released_inputs"

// recordReleased writes whether this read held a released value. A response
// with no private state, as a unit test may build one, records nothing.
func recordReleased(ctx context.Context, private privateWriter, held bool) diag.Diagnostics {
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return nil
	}
	var raw []byte
	if held {
		raw = []byte("true")
	}
	return private.SetKey(ctx, releasedKey, raw)
}

// wasReleased reports whether the last read held a released value.
func wasReleased(ctx context.Context, private privateState) bool {
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return false
	}
	raw, diags := private.GetKey(ctx, releasedKey)
	return !diags.HasError() && string(raw) == "true"
}

// vpcTeardownMemo records the VPC a deleting owner stopped reporting in this
// provider run. The owner's data sources depend on its vpc attribute, so its
// read records the held id first.
type vpcTeardownMemo struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func vpcTeardownKey(target clusterTarget, id string) string {
	return target.region + "/" + target.project + "/" + id
}

func (m *vpcTeardownMemo) record(target clusterTarget, id string, held bool) {
	if id == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := vpcTeardownKey(target, id)
	if !held {
		delete(m.ids, key)
		return
	}
	if m.ids == nil {
		m.ids = map[string]struct{}{}
	}
	m.ids[key] = struct{}{}
}

func (m *vpcTeardownMemo) held(target clusterTarget, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.ids[vpcTeardownKey(target, id)]
	return ok
}

// holdVPCThroughTeardown keeps the id a released owner stopped reporting and
// lets its data source tolerate the VPC's 404 until this run ends. A live
// owner's missing VPC remains drift and a missing id remains a data error.
func holdVPCThroughTeardown(meta *Meta, target clusterTarget, released bool, read, prior types.String) types.String {
	value, held := heldThroughTeardown(released, read.IsNull(), read, prior)
	meta.vpcTeardown.record(target, prior.ValueString(), held)
	return value
}

// warnHeldVPCWrite warns when a planned create or change names a VPC this
// run saw its owner release. Terraform's pre-destroy refresh is a normal
// plan, so an error here would also stop the destroy that ends the teardown.
// The write itself fails at apply with addMissingVPCError.
func warnHeldVPCWrite(ctx context.Context, meta *Meta, req resource.ModifyPlanRequest,
	resp *resource.ModifyPlanResponse, attributes ...string) {
	if meta == nil || req.Plan.Raw.IsNull() || req.Plan.Raw.Equal(req.State.Raw) {
		return
	}
	var target clusterTarget
	for name, into := range map[string]*string{"region": &target.region, "project": &target.project} {
		var v types.String
		if resp.Plan.GetAttribute(ctx, path.Root(name), &v).HasError() {
			return
		}
		value, ok := known(v)
		if !ok {
			return
		}
		*into = value
	}
	for _, attribute := range attributes {
		var v types.String
		if resp.Plan.GetAttribute(ctx, path.Root(attribute), &v).HasError() {
			continue
		}
		id, ok := known(v)
		if !ok {
			continue
		}
		at := target
		if attribute == "remote_vpc" {
			p, ok := vpcRoute.params(remoteVPCURI(target, id))
			if !ok {
				continue
			}
			at, id = clusterTarget{region: p["region"], project: p["project"]}, p["id"]
		}
		if !meta.vpcTeardown.held(at, id) {
			continue
		}
		resp.Diagnostics.AddAttributeWarning(path.Root(attribute),
			"VPC gone while its owner is being torn down",
			fmt.Sprintf("VPC %q in project %q of region %q is gone because the cluster or instance group "+
				"that owned it is being torn down. Applying this plan will fail for this resource. "+
				"Run `terraform destroy` to finish the teardown.", id, at.project, at.region))
	}
}

// missingVPCIn is the VPC a refused write shows doesn't exist, with the
// attribute that names it, or "" when the answer doesn't show that. The
// service's 404 on a write under a VPC means the VPC in the path is missing.
// A missing remote VPC is a 422 whose reason other refusals share, so the
// platform's message is what tells it apart.
func missingVPCIn(resp *http.Response, body []byte, local, remote string) (id, attribute string) {
	if isNotFound(resp, body) {
		return local, "vpc"
	}
	apiErr := apiErrorOf(resp, body)
	if remote != "" && apiErr != nil && apiErr.StatusCode == http.StatusUnprocessableEntity &&
		strings.Contains(apiErr.Message, fmt.Sprintf("remoteVpc %q not found", remote)) {
		return remote, "remote_vpc"
	}
	return "", ""
}

// addMissingVPCError reports a write refused because the VPC it names
// doesn't exist, and reports whether it was one. A VPC goes only with its
// cluster or instance group, so it says how to finish that teardown.
func addMissingVPCError(diags *diag.Diagnostics, what apiCall, target clusterTarget,
	resp *http.Response, body []byte, local, remote string) bool {
	remoteAt := target
	if remote != "" {
		p, ok := vpcRoute.params(remoteVPCURI(target, remote))
		if !ok {
			remote = ""
		} else {
			remoteAt, remote = clusterTarget{region: p["region"], project: p["project"]}, p["id"]
		}
	}
	id, attribute := missingVPCIn(resp, body, local, remote)
	if id == "" {
		return false
	}
	at := target
	if attribute == "remote_vpc" {
		at = remoteAt
	}
	diags.AddError(fmt.Sprintf("Could not %s because VPC %q doesn't exist", what, id),
		apiErrorDetailNext(apiErrorOf(resp, body), fmt.Sprintf("VPC %q doesn't exist in project %q of "+
			"region %q. If the cluster or instance group that owned it is being torn down, run "+
			"`terraform destroy` to finish the teardown. Otherwise check the VPC id in `%s`.",
			id, at.project, at.region, attribute)))
	return true
}
