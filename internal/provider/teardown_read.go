package provider

import (
	"context"
	"reflect"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// What a read keeps while the platform tears a resource down.
//
// Some kinds stop reporting part of what their create sent once a delete
// starts. The platform releases it in the same transaction that moves the
// resource to `deleting`, so every read after that answers it empty: a
// group's SSH users, and a cluster's node pools with `"nodeCount":0`. A teardown that fails settles the
// resource `failed` with them still gone, since nothing puts them back.
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
