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

// forbiddenGuidance is the next step for a 403 on each read, and each project
// create or delete, that only a role bound to an organization, or to the whole
// platform, can make. The platform checks these routes against the
// organization, and a role bound to a project never counts there, so a
// credential scoped to a project is refused all of them. Its 403 says only
// "forbidden". The roles are the platform's built-in ones that hold each
// route's permission, as the k0rdent API v1.3.0-rc.3 binds them, and as
// v1.3.0-rc.4 binds a project's create and delete.
var forbiddenGuidance = func() map[string]string {
	// The gateway answers the same bare 403 for an organization that doesn't
	// exist, so each message names that too.
	const (
		noSuchOrg    = "and the platform gives this same 403 for an organization id that doesn't exist."
		organization = "Reading an organization needs organizations-viewer, or a higher organizations role, " +
			"on it, " + noSuchOrg + " A credential scoped to a project can't have that role, but `%s` " +
			"lists the organizations it can see, with their status."
		project = "Reading a project through its organization needs organizations-viewer or compute-viewer, " +
			"or higher, on the organization, " + noSuchOrg + " A role on the project doesn't count, so the " +
			"`%s` data source reads a project-scoped credential's own project from the project list instead."
		quota = "Reading quota needs organizations-viewer, or a higher organizations role, on the " +
			"organization, " + noSuchOrg + " A credential scoped to a project can't have that role, so use " +
			"one scoped to the organization or leave this read out."
		roles = "Listing roles needs iam-viewer or iam-admin on the organization, " + noSuchOrg +
			" A credential scoped to a project can't have either, so use one scoped to the organization " +
			"or leave this read out."
		accounts = "Reading service accounts needs iam-admin on the organization, not iam-viewer, " +
			noSuchOrg + " A credential scoped to a project can't have it, so use one scoped to the " +
			"organization or leave this read out."
		// organizations-member can create a project and can't delete one.
		createProject = "Creating a project needs organizations-member or organizations-admin on the " +
			"organization, " + noSuchOrg + " Only organizations-admin can delete it again, so a destroy " +
			"needs that role, on a credential scoped to the organization."
		deleteProject = "Deleting a project needs organizations-admin on the organization, not " +
			"organizations-member, " + noSuchOrg + " A credential scoped to a project can't have it, so " +
			"delete the project with one scoped to the organization."
	)
	return map[string]string{
		"getOrganization":               fmt.Sprintf(organization, ResourceName(organizationsDataSourceSuffix)),
		"getProjectInOrg":               fmt.Sprintf(project, ResourceName(projectDataSourceSuffix)),
		"getProjectQuota":               quota,
		"getTenantQuota":                quota,
		"listRoles":                     roles,
		"getServiceAccount":             accounts,
		"listServiceAccounts":           accounts,
		"listServiceAccountCredentials": accounts,
		"createProjectInOrg":            createProject,
		"deleteProjectInOrg":            deleteProject,
	}
}()

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
	// A 403 with a reason is a refusal of its own kind, guided above or not
	// at all. Without one, it's the missing role.
	if apiErr.StatusCode == http.StatusForbidden && apiErr.Reason == "" {
		if g, ok := forbiddenGuidance[apiErr.Operation]; ok {
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
