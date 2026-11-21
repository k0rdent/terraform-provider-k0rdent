package provider

import (
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// createTokenAttribute is the durable create token's attribute name.
const createTokenAttribute = "create_token"

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
			"This is proof of provenance, not a secret credential. The platform returns the tag on reads. " +
			"Terraform passes the value to Create on each run but never writes it to plan or state. Use " +
			"an ephemeral input variable so a saved plan's apply also receives it without persisting the " +
			"plan file. Removing this attribute affects only future creates. An existing cluster keeps " +
			"its tag. Never sent except in that tag.",
	}
}
