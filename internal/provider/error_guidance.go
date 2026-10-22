package provider

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// What to do about a refusal.
//
// The platform's message says what went wrong, and a reader then has to know
// what to change. For every reason a provider operation can be refused with,
// reasonGuidance holds that next step in a sentence or two. apiErrorDetail adds
// it after the platform's own text and before the request id, so nothing the
// platform said is replaced.
//
// Reasons the contract documents only for operations this provider never
// calls, an invitation's DOMAIN_NOT_OWNED or an identity provider's
// INVALID_DISCOVERY_URL, aren't listed: guidance for an answer it can't get
// would be untested text.

// reasonGuidance is the next step for each known reason, top-level or in a
// detail.
var reasonGuidance = map[string]string{
	"NON_INTERACTIVE_PRINCIPAL": "The platform lets only a person create credentials or change access " +
		"policies, and the provider signs in as a service account. Make this change as a person in the " +
		"console, or take it out of this configuration.",
	"INSUFFICIENT_CAPACITY": "The platform has no room for what was asked. For a subnet, the pool has no " +
		"free block that size: ask for a smaller one with a larger `prefix_length`, or choose another " +
		"`address_pool`. A routable pool also holds cluster VIPs, so a non-routable pool is the usual fix. " +
		"For compute, choose another size or region, or apply again later.",
	"CONFLICT_BACKEND_CAPACITY_EXCEEDED": "The storage behind this share type has no capacity left. " +
		"Choose another `share_type` or a smaller `capacity_gib`, or report it with the request id.",
	"CONFLICT_CAPACITY_BELOW_USAGE": "The new `capacity_gib` is below what the store already holds. " +
		"Set it at or above the store's usage, or free space first.",
	"CONFLICT_IMMUTABLE": "The platform won't change this field in place. Put it back to the value in " +
		"state, or replace the resource with `terraform apply -replace=ADDRESS`.",
	"QUOTA_EXCEEDED": "Ask for less, free usage elsewhere, or have the binding limit raised. Applying " +
		"again unchanged gets the same answer.",
	"QUOTA_UNASSIGNED": "The organization has no quota profile, and the platform requires one. An " +
		"operator has to assign it. Applying again won't help until then.",
}

// platformBuiltKinds are objects the platform builds for itself while it
// serves a request. The tenant API has no such object, so a refusal naming one
// is about something no configuration sets. A cluster create once answered
// 400 "prefixLength must be at most 31" about a VPCPrefix it made.
var platformBuiltKinds = []string{"VPCPrefix"}

// reportIt is the next step when the fault is on the platform's side.
const reportIt = "Report it to the platform team with the request id."

// guidanceFor is the next step for a refusal, or "" when nothing more can be
// said than the platform's message. A top-level reason wins over a detail's,
// and a reason over the status.
func guidanceFor(apiErr *k0rdent.APIError) string {
	reasons := append([]string{apiErr.Reason}, apiErr.DetailReasons()...)
	for _, r := range reasons {
		if g, ok := reasonGuidance[r]; ok {
			return g
		}
	}
	// The transport's lookup, when it ran, has said this already and named
	// the region. See region_mismatch.go.
	if apiErr.HasReason(regionMismatch) {
		if regionMismatchAdvice(apiErr) != "" {
			return ""
		}
		return fmt.Sprintf("Set `region` to the project's region. The `%s` data source reports it.",
			ResourceName(projectDataSourceSuffix))
	}
	for _, kind := range platformBuiltKinds {
		if strings.Contains(apiErr.Message, kind) {
			return fmt.Sprintf("%s is an object the platform builds for itself, and nothing in this "+
				"configuration sets it, so the platform has to fix it. %s", kind, reportIt)
		}
	}
	if apiErr.StatusCode >= http.StatusInternalServerError {
		return "The platform failed, not the configuration. Apply again later. If it keeps failing, " +
			"report it to the platform team with the request id."
	}
	return ""
}
