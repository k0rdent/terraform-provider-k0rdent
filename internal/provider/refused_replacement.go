package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// A replacement whose create the apply would refuse keeps the object it
// replaces.
//
// Terraform plans a tainted object's replacement as a create, with no prior
// state, so a create check only warns there, and `terraform destroy` still
// runs. The apply deletes the old object first, and Create's refusal would
// then leave nothing. Terraform hands the Delete that starts a replacement
// the create plan's planned private state, and a destroy's Delete the
// destroy plan's. So each create plan keeps, under replacementChecksKey, the
// values its create checks were given, and Delete makes those checks again,
// live, before it sends anything. A check that fails there refuses the
// delete and keeps the object. One that passes, or can't decide, lets the
// delete go ahead, as Create would.
//
// Every plan starts with the key cleared, a destroy's included, so
// `terraform destroy` always deletes. A refused Delete clears it too, since
// Terraform saves a failed delete's private state. Create starts from empty
// private state, so it never saves the key.

// replacementChecksKey is the private-state key the checks are kept under.
const replacementChecksKey = "replacement_checks"

// replacementCheck is one create check and the values the plan gave it.
type replacementCheck struct {
	// Kind names the check: "region" for a resource's region against its
	// project's.
	Kind    string `json:"kind"`
	Region  string `json:"region,omitempty"`
	Project string `json:"project,omitempty"`
}

// privateStore is a request's or response's private state, read and written.
type privateStore interface {
	privateState
	privateWriter
}

// absent reports whether private is missing: a nil interface, or a nil
// pointer the framework never set up, which a unit test can hand in.
func absent(private any) bool {
	if private == nil {
		return true
	}
	v := reflect.ValueOf(private)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// clearReplacementChecks drops the checks from private.
func clearReplacementChecks(ctx context.Context, private privateStore, diags *diag.Diagnostics) {
	if absent(private) {
		return
	}
	if raw, _ := private.GetKey(ctx, replacementChecksKey); len(raw) == 0 {
		return
	}
	diags.Append(private.SetKey(ctx, replacementChecksKey, nil)...)
}

// recordReplacementCheck adds c to the checks a create plan keeps.
func recordReplacementCheck(ctx context.Context, private privateStore, c replacementCheck, diags *diag.Diagnostics) {
	if absent(private) {
		return
	}
	checks := replacementChecksIn(ctx, private, diags)
	raw, err := json.Marshal(append(checks, c))
	if err != nil {
		return // a json.Marshal of strings can't fail
	}
	diags.Append(private.SetKey(ctx, replacementChecksKey, raw)...)
}

// replacementChecksIn reads the checks in private. Something unreadable
// there is treated as none, which at worst lets a delete go ahead as it did
// before these checks.
func replacementChecksIn(ctx context.Context, private privateState, diags *diag.Diagnostics) []replacementCheck {
	if absent(private) {
		return nil
	}
	raw, d := private.GetKey(ctx, replacementChecksKey)
	diags.Append(d...)
	var checks []replacementCheck
	if len(raw) == 0 || json.Unmarshal(raw, &checks) != nil {
		return nil
	}
	return checks
}

// replacementRefusal is the summary of a delete refused because the create
// after it would be.
const replacementRefusal = "This replacement would be refused, so nothing was deleted"

// refuseRefusedReplacement makes a create plan's checks again before the
// delete that starts its replacement, and refuses the delete when one fails,
// before anything is sent. It reports whether the delete stops here, having
// said why.
func refuseRefusedReplacement(ctx context.Context, meta *Meta, private privateState, out privateStore, diags *diag.Diagnostics) bool {
	checks := replacementChecksIn(ctx, private, diags)
	if len(checks) == 0 {
		return false
	}
	clearReplacementChecks(ctx, out, diags)
	var problems []string
	for _, c := range checks {
		switch c.Kind {
		case "region":
			if home, ok := regionMismatchOf(ctx, meta, c.Region, c.Project); ok {
				problems = append(problems, regionMismatchText(c.Project, home, c.Region))
			}
		}
	}
	if len(problems) == 0 {
		return false
	}
	// Local: the create's own checks, made before the delete is sent.
	diags.AddError(replacementRefusal, redactDiagnosticText("Terraform planned to replace this resource, "+
		"deleting it and then creating it, and the provider would refuse the create. "+
		strings.Join(problems, " ")+" So this apply sent nothing for this resource, and it's still on the "+
		"platform and in state. Terraform skips the resources that depend on it and applies the rest of the "+
		"plan. To replace it, fix the configuration and apply again. To keep it as it is, run `terraform "+
		"untaint` if it's tainted. `terraform destroy` still deletes it."))
	return true
}
