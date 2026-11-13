package provider

import "fmt"

// Refusals that warn at plan and fail at apply.
//
// `terraform destroy` runs a normal plan before it plans the delete, and an
// error there stops the destroy. The provider can't tell that plan from any
// other. So a change that deletes nothing, an update in place or a create,
// is only warned of at plan, and the apply refuses it before it sends
// anything. A replacement is refused at plan still: its delete comes before
// the create's refusal, so refusing at apply would leave the resource
// deleted.
//
// When one resource's apply fails, Terraform skips the resources that depend
// on it and applies the rest of the plan, so the texts below say so.

// checkedAtUpdate ends the plan's warning of an update the apply refuses.
// what names the resource, such as "file store".
func checkedAtUpdate(what string) string {
	return fmt.Sprintf(" The apply checks this again. If it still holds, the apply fails before it sends "+
		"any request for this %[1]s, so none of its changes in this plan are made, and Terraform skips the "+
		"resources that depend on it. The rest of the plan is applied. `terraform destroy` isn't affected.", what)
}

// updateSentNothing ends the apply's refusal of an update.
func updateSentNothing(what string) string {
	return fmt.Sprintf(" This apply sent nothing for this %[1]s, so it's unchanged. Terraform skips the "+
		"resources that depend on it and applies the rest of the plan.", what)
}

// checkedAtCreate ends the plan's warning of a create the apply refuses.
const checkedAtCreate = " The apply checks this again. If it still holds, the apply fails before it sends " +
	"the create, so this resource isn't created, and Terraform skips the resources that depend on it. The " +
	"rest of the plan is applied. If this replaces a tainted object, Terraform deletes that object first and " +
	"nothing takes its place: run `terraform untaint` to keep it. `terraform destroy` isn't affected."

// createSentNothing ends the apply's refusal of a create.
const createSentNothing = " This apply sent nothing to create this resource, so it doesn't exist. Terraform " +
	"skips the resources that depend on it and applies the rest of the plan. If it was to replace a tainted " +
	"object, Terraform deleted that object first."

// replacedOnThisPlan ends the plan's refusal of a replacement.
const replacedOnThisPlan = " This plan replaces the resource, and Terraform would delete it before the apply " +
	"could refuse the new one, so the plan fails here and nothing is sent. This error stops `terraform " +
	"destroy` too: undo the change that replaces the resource, or run `terraform destroy -refresh=false`, " +
	"which destroys from state as it is."
