package provider

import (
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// createTokenAttribute is the durable create token's attribute name.
const createTokenAttribute = "create_token"

// adoptOnlyAttribute controls recovery without creation on the ordinary managed resource.
const adoptOnlyAttribute = "adopt_only"

// adoptOnlySchemaAttribute makes absence a clean no-op and provenance a
// precondition for recording an existing cluster.
func adoptOnlySchemaAttribute() schema.BoolAttribute {
	return schema.BoolAttribute{
		Optional: true,
		MarkdownDescription: "Adopt an existing cluster by `create_token` without ever creating one. " +
			"Requires a non-empty `create_token`. Use the `" + ResourceName(clusterDataSourceSuffix+"s") + "` lookup with import and " +
			"resource `for_each`, as the recovery guide shows, so absence plans no managed instance. An exact " +
			"match records the platform UID without creating a cluster. The plan keeps the observed values " +
			"and warns about configured changes, including a different `cluster_type` version. " +
			"Missing or different provenance is refused. A later destroy uses the recorded UID, so a " +
			"replacement under the same id is never adopted or deleted.",
	}
}

// createTokenNonBlankValidator rejects blank provenance without changing any
// nonblank byte, since the platform tag comparison is exact.
func createTokenNonBlankValidator() validator.String {
	return stringvalidator.RegexMatches(regexp.MustCompile(`\S`), "must contain at least one non-whitespace character")
}

// createTokenSchemaAttribute declares `create_token`. It is provenance, not a
// credential, so the platform can return it as a tag after the runner is gone.
// Write-only keeps it out of plan and state. Configuration supplies it on every
// run, which is how a later Create can ask the existing 409 readback whether
// this is the same intended creation.
func createTokenSchemaAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional:   true,
		WriteOnly:  true,
		Validators: []validator.String{createTokenNonBlankValidator()},
		MarkdownDescription: "An optional durable token that proves a cluster create belongs to this " +
			"configuration. Generate it before the first apply and pass the same value on every retry. " +
			"The provider sends it in its own `terraform-provider-k0rdent/create-token` tag instead of " +
			"making a per-call random token. If a runner dies after the platform accepts a create and " +
			"before Terraform saves state, the next apply reads the configured `id` in the configured " +
			"`region` and `project` and adopts the cluster only when this token exactly matches. It " +
			"records the platform UID, and a missing or different token is refused. A supplied empty or " +
			"whitespace-only string is refused rather than silently reverting to a per-call token.\n\n" +
			"A run interrupted while the create waits, such as by a CI cancel or a job timeout, records the " +
			"cluster untainted with the last state it read, as `on_create_timeout = \"keep\"` does for a " +
			"timeout. So the next apply waits for the same cluster rather than replacing it.\n\n" +
			"This is proof of provenance, not a secret credential. The platform returns the tag on reads. " +
			"Terraform passes the value to Create on each run but never writes it to plan or state. Use " +
			"an ephemeral input variable so a saved plan's apply also receives it without persisting the " +
			"plan file. Removing this attribute affects only future creates. An existing cluster keeps " +
			"its tag. Never sent except in that tag.",
	}
}
