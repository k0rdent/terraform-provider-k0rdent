package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

// Which object a row is for.
//
// An id names an object only while it lives. The contract lets a create take
// the id of a deleted cluster, instance group, project or security group, and
// says nothing either way for the other kinds, so a row bound to the id alone
// would refresh into whatever another apply made under it later, and a
// destroy would delete that. The uid is never reused, so every kind with a
// configured id keeps the uid of the object its row was made for, and a
// refresh that finds another one under the id drops the row rather than
// adopting it. A webhook subscription's id is the platform's own UUID, so it
// needs none of this.
//
// A row with none recorded takes the first uid it reads: one an import or an
// earlier version wrote, and a create whose wait failed or whose answer
// couldn't be confirmed, which records the row before it has read the object.
//
// The check also rests on a live object keeping its uid. The contract says
// so for a file store and a security group, and calls the uid the handle to
// record for the rest. If a kind ever changed it, its rows would drop with
// this warning and need an import, and nothing would be deleted.

// uidKey is the private state key the row's uid is kept under.
const uidKey = "uid"

// recordUID keeps uid as the object this row is for. Terraform always hands a
// create and a read their private state. A test that calls one directly may
// not, and there's nothing to record into.
func recordUID(ctx context.Context, private privateWriter, uid string) diag.Diagnostics {
	if v := reflect.ValueOf(private); uid == "" || !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return nil
	}
	raw, err := json.Marshal(uid)
	if err != nil {
		return nil
	}
	return private.SetKey(ctx, uidKey, raw)
}

// heldObject reports whether uid is the object this row is for, and the uid
// the row holds. A row that holds none, or a read with no uid, is the same.
func heldObject(ctx context.Context, private privateState, uid string) (string, bool) {
	if v := reflect.ValueOf(private); uid == "" || !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return "", true
	}
	raw, diags := private.GetKey(ctx, uidKey)
	var held string
	if diags.HasError() || len(raw) == 0 || json.Unmarshal(raw, &held) != nil || held == "" {
		return "", true
	}
	return held, held == uid
}

// addAnotherObject warns that the row was dropped because id now names
// another object than the one it was made for.
func addAnotherObject(diags *diag.Diagnostics, kind, id, was, now string) {
	diags.AddWarning(
		fmt.Sprintf("The %s %q is another %s now", kind, id, kind),
		fmt.Sprintf("This state's %s %q has uid %s. The platform's %s under that id has uid %s, so the one "+
			"this state was for was deleted, and another was created under the same id. It was dropped from "+
			"state rather than adopted, so Terraform won't change or delete it. To manage it from this "+
			"configuration, import it.", kind, id, was, kind, now),
	)
}

// refreshedOwnObject is the check a Read makes once it has found the object:
// whether uid is the one the row is for. It records uid when it is, and
// otherwise warns, drops the row, and reports false.
func refreshedOwnObject(ctx context.Context, held privateState, resp *resource.ReadResponse, kind, id, uid string) bool {
	if was, same := heldObject(ctx, held, uid); !same {
		addAnotherObject(&resp.Diagnostics, kind, id, was, uid)
		resp.State.RemoveResource(ctx)
		return false
	}
	resp.Diagnostics.Append(recordUID(ctx, resp.Private, uid)...)
	return true
}

// uidOf is a uid the platform answered, or "" for none.
func uidOf(u *openapi_types.UUID) string {
	if u == nil {
		return ""
	}
	return u.String()
}
