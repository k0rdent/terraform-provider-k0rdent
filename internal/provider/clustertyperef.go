package provider

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// versionsSegment is the optional path element that pins a ClusterType version.
const versionsSegment = "/versions/"

// clusterTypeRefMatch says whether a `clusterType` value returned by the server
// is the same binding the user asked for, and how it differed.
type clusterTypeRefMatch int

const (
	// refMismatch means the two values name different things. Real drift.
	refMismatch clusterTypeRefMatch = iota
	// refIdentical means the server echoed the value byte for byte.
	refIdentical
	// refSameOrigin means the two differ only in scheme and host: one is
	// absolute, the other a path.
	refSameOrigin
	// refVersionResolved means the server added a `/versions/{id}` segment to
	// the unversioned value the user wrote.
	refVersionResolved
)

// clusterTypeRefEquivalent compares the `clusterType` a user wrote against the
// value the server returned and reports whether they name the same binding.
//
// `clusterType` is immutable after create and is the one field a user types by
// hand, so a value that does not read back identical proposes a change on every
// plan forever, on a field that cannot be changed. Two differences are
// legitimate and a plain `==` rejects both: the server may answer with a
// relative path where an absolute URL was sent, and it may resolve an
// unversioned reference to a pinned `/versions/{id}` one.
//
// Nothing else is tolerated. Any other difference is a mismatch, which produces
// the replacement a genuinely re-bound cluster deserves. The match kind is
// returned so the caller can surface the resolved value in
// `cluster_type_resolved` rather than hide it.
func clusterTypeRefEquivalent(configured, returned string) clusterTypeRefMatch {
	if configured == returned {
		return refIdentical
	}

	wantPath, wantOrigin := refPath(configured)
	gotPath, gotOrigin := refPath(returned)
	if wantPath == "" || gotPath == "" {
		return refMismatch
	}

	// A differing host is a different binding, an https-to-http downgrade
	// included. Comparing the origins, not just whether each side carried one,
	// is what catches that. One relative side is the documented normalization.
	sameOrigin := wantOrigin == gotOrigin || wantOrigin == "" || gotOrigin == ""

	if wantPath == gotPath {
		switch {
		case wantOrigin == gotOrigin:
			return refIdentical
		case sameOrigin:
			return refSameOrigin
		default:
			return refMismatch
		}
	}

	// The user wrote the head form and the server answered with a version
	// pinned under it. Accepted in that direction only: a pinned version
	// answered with the head has dropped the pin, which is a different binding.
	// The origin rule holds here too, or a pinned answer from another host
	// would read as the configured binding.
	if rest, ok := strings.CutPrefix(gotPath, wantPath+versionsSegment); ok && rest != "" && !strings.Contains(rest, "/") && sameOrigin {
		return refVersionResolved
	}

	return refMismatch
}

// clusterTypeRefReplacement decides when a changed `cluster_type` means the
// resource has to be replaced.
//
// A plain `RequiresReplace()` destroys running clusters here. On import there is
// no configuration to reconcile against, so the resource adopts whatever the
// platform reported; when that is a resolved `/versions/{id}` reference and the
// configuration holds the unversioned one, the next plan proposes replacing a
// running cluster with nobody having changed anything. That was observed, not
// predicted.
//
// So replacement turns on whether the two references name the same binding, not
// on whether the strings match. Equivalent values converge in place with no API
// call. Genuinely different ones replace, which is correct: the binding cannot
// be changed. `cluster_type_resolved` shows the resolved form either way.
//
// A cluster is the exception for another version of the same ClusterType,
// which the platform reaches by upgrading in place: see
// clusterTypeVersionGuard.
func clusterTypeRefReplacement() planmodifier.String {
	const desc = "replaced only when the new reference names a different ClusterType binding, " +
		"not merely a different spelling of the same one"
	return stringplanmodifier.RequiresReplaceIf(clusterTypeRefRequiresReplace, desc, desc)
}

// clusterTypeVersionGuard is clusterTypeRefReplacement for a cluster. Another
// version of the same ClusterType is never a replacement, because replacing
// would destroy the cluster to change its version. Whether it is an upgrade or
// a refusal needs the version list, so the resource's ModifyPlan decides.
func clusterTypeVersionGuard() planmodifier.String {
	const desc = "replaced only when the new reference names a different ClusterType, and " +
		"another version of the same one is upgraded in place or refused"
	return stringplanmodifier.RequiresReplaceIf(func(ctx context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
		clusterTypeRefRequiresReplace(ctx, req, resp)
		if resp.RequiresReplace {
			// The binding the platform reported, as planVersionChange and the
			// apply compare with, so the three agree on another version.
			bound := req.StateValue.ValueString()
			var resolved types.String
			req.State.GetAttribute(ctx, path.Root("cluster_type_resolved"), &resolved)
			if r := resolved.ValueString(); r != "" {
				bound = r
			}
			change, _ := classifyRefChange(req.ConfigValue.ValueString(), bound, "")
			resp.RequiresReplace = change == refReplace
		}
	}, desc, desc)
}

// sameClusterType reports whether two references name one ClusterType, at any
// version or none.
func sameClusterType(a, b string) bool {
	pa, oa := refPath(a)
	pb, ob := refPath(b)
	baseA, _, _ := strings.Cut(pa, versionsSegment)
	baseB, _, _ := strings.Cut(pb, versionsSegment)
	return pa != "" && pb != "" && baseA == baseB && (oa == ob || oa == "" || ob == "")
}

func clusterTypeRefRequiresReplace(ctx context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy
	}
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() || req.StateValue.IsNull() {
		return
	}
	configured, stored := req.ConfigValue.ValueString(), req.StateValue.ValueString()
	if configured == stored {
		return
	}
	if clusterTypeRefEquivalent(configured, stored) != refMismatch {
		return
	}
	// The stored value may be the unversioned spelling an import chose while
	// the configuration pins the very version it resolved to. The binding the
	// platform reported is the one to compare with, and the same binding
	// spelled differently is never a replacement.
	var resolved types.String
	req.State.GetAttribute(ctx, path.Root("cluster_type_resolved"), &resolved)
	if r := resolved.ValueString(); r != "" && clusterTypeRefEquivalent(configured, r) != refMismatch {
		return
	}
	resp.RequiresReplace = true
}

// refPath reduces a reference to its path and its origin, the origin being
// empty for a relative reference. Query strings and fragments are not part of a
// resource identity here; a reference carrying either is treated as unparseable
// rather than silently trimmed, because nothing in the contract produces one.
func refPath(ref string) (path, origin string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", ""
	}
	path = strings.TrimSuffix(u.Path, "/")
	if path == "" {
		return "", ""
	}
	if u.Host == "" {
		return path, ""
	}
	// A hostname is case-insensitive and the default port is not part of the
	// origin, so neither spelling names a different ClusterType. Comparing them
	// raw replaces a running cluster over a difference nobody made.
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	switch scheme {
	case "https":
		host = strings.TrimSuffix(host, ":443")
	case "http":
		host = strings.TrimSuffix(host, ":80")
	}
	return path, scheme + "://" + host
}

// clusterTypeURIRegion reports the region a reference's path names, and
// false when the path is not a form the platform's create accepts.
//
// The rule is the platform's own, so the provider refuses at plan exactly
// what a create would refuse at apply: the path is
// /v1/regions/{region}/compute/cluster-types/{id}, where the region and the
// id have no `/` and are not empty, with an optional /versions/{version_id}
// whose id is not empty and has no `/` either. A bare ClusterType name, a
// missing `/v1`, a `/versions/` with nothing after it and an extra segment
// are all refused. The path is what the value itself carries, as
// clusterTypeSentPath reads it, so an absolute URL whose path has this form
// passes, the way the comparison code reads one.
func clusterTypeURIRegion(path string) (region string, ok bool) {
	const middle = "/compute/cluster-types/"
	const versions = "/versions/"
	rest, ok := strings.CutPrefix(path, clusterTypeRefRegionsPrefix)
	// The platform's parse would take a query or a fragment as part of the
	// id, which no ClusterType has, so the create fails on it either way.
	if !ok || strings.ContainsAny(rest, "?#") {
		return "", false
	}
	region, tail, _ := strings.Cut(rest, middle)
	slug, version, pinned := strings.Cut(tail, versions)
	if pinned && (version == "" || strings.Contains(version, "/")) {
		return "", false
	}
	if region == "" || slug == "" || strings.Contains(region, "/") || strings.Contains(slug, "/") {
		return "", false
	}
	return region, true
}

// clusterTypeSentPath reports the path a `cluster_type` value carries when
// the create sends it verbatim, and false when the value isn't URL-shaped
// enough to have one. Unlike refPath it doesn't tidy the value: the
// platform's create parses what was sent, trailing slash and all, so a
// path the tidy form would accept can still be one the create refuses.
func clusterTypeSentPath(ref string) (string, bool) {
	u, err := url.Parse(ref)
	if err != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return u.Path, true
}

// clusterTypeCreateRef is the `clusterType` a create sends: the path of an
// absolute URL, the value as written otherwise. The platform's create parses
// only the path form, and the provider already reads a reference's origin as
// no part of which ClusterType it names, so sending the path keeps an
// absolute URL working on a create as it does on an update.
func clusterTypeCreateRef(ref string) string {
	if p, ok := clusterTypeSentPath(ref); ok && p != ref {
		return p
	}
	return ref
}

// clusterTypeRefForm refuses a `cluster_type` value the platform's create
// answers with 422 "clusterType is not a valid ClusterType URI", at plan
// rather than after the plan was approved.
//
// The contract types the field `uri-reference`, which holds any reference,
// so the contract itself refuses nothing here. The platform's create does:
// it reads only the form clusterTypeRefFormText names, and a bare
// ClusterType name reaches it whole and is refused there, after other
// resources in the plan may already have been made.
type clusterTypeRefForm struct{}

func (clusterTypeRefForm) Description(context.Context) string {
	return "must be a ClusterType reference, " + clusterTypeRefFormText
}

func (v clusterTypeRefForm) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (clusterTypeRefForm) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	ref := req.ConfigValue.ValueString()
	if p, parseable := clusterTypeSentPath(ref); parseable {
		if _, ok := clusterTypeURIRegion(p); ok {
			return
		}
	}
	// Local: configuration validation, no request made.
	resp.Diagnostics.AddAttributeError(req.Path, "Not a ClusterType reference",
		fmt.Sprintf("%q is not a ClusterType reference. A reference is %s. "+
			"Use the `self_link` of the `%s` data source, which has that form in the region it read.",
			ref, clusterTypeRefFormText, ResourceName(clusterTypeDataSourceSuffix)))
}

// refuseClusterTypeRegionMove fails a plan whose `cluster_type` names a
// region other than the one the resource resolves to, when both are known.
//
// The platform's create refuses a cluster type from another region with
// 422 "clusterType region ... does not match ... region", after the plan
// approved it. Run at plan, on the region the scope has just resolved into
// it, the same refusal names the way out before anything is sent. Skipped
// whenever either value is unknown: a `cluster_type` a data source fills
// during the apply can't be checked yet, and neither can a region that
// isn't resolved.
func refuseClusterTypeRegionMove(ctx context.Context, plan tfsdk.Plan, diags *diag.Diagnostics, kind string) {
	var clusterType, region types.String
	if plan.GetAttribute(ctx, path.Root("cluster_type"), &clusterType).HasError() ||
		plan.GetAttribute(ctx, path.Root("region"), &region).HasError() {
		return
	}
	if clusterType.IsNull() || clusterType.IsUnknown() || region.IsNull() || region.IsUnknown() {
		return
	}
	p, _ := clusterTypeSentPath(clusterType.ValueString())
	uriRegion, ok := clusterTypeURIRegion(p)
	if !ok || uriRegion == region.ValueString() {
		return
	}
	diags.AddAttributeError(path.Root("cluster_type"), "The cluster type is in another region",
		fmt.Sprintf("`cluster_type` %q names the region %q, but this %s's region is %q. "+
			"The platform refuses a cluster type from another region, so the create would fail "+
			"after the plan approved it. Set `region` to %q, or use the `self_link` of the "+
			"`%s` data source read in %q. The plan was refused, so nothing was sent.",
			clusterType.ValueString(), uriRegion, kind, region.ValueString(), uriRegion,
			ResourceName(clusterTypeDataSourceSuffix), region.ValueString()))
}

// refTypeAndVersion splits a reference into its ClusterType id and its version
// id, the second empty for an unversioned reference.
func refTypeAndVersion(ref string) (typeID, versionID string) {
	p, _ := refPath(ref)
	base, versionID, _ := strings.Cut(p, versionsSegment)
	return base[strings.LastIndex(base, "/")+1:], versionID
}

// importedClusterTypeRef chooses how an import records `cluster_type`.
//
// The platform answers with the version it bound, `.../versions/{id}`, and an
// import has no configuration to say which spelling was written. The provider's
// own examples write the unversioned form, a cluster type's `self_link`, which
// a create binds to the newest `available` version. So when the bound version
// is still that one, the import records the unversioned form, and a
// configuration written the documented way plans no change. A pin to anything
// else is kept as the platform reported it, because then the unversioned form
// would mean a different version. `cluster_type_resolved` records the pin
// either way.
//
// "Current" comes from the version list, not from the ClusterType's own
// versionId. The contract says getClusterType resolves to the newest row
// whatever its status, a draft included, which is not what a create binds.
//
// The trade is one extra read on import, and a configuration that pins the
// current version explicitly plans an in-place update to the spelling after
// the import. A failed read keeps the pin, which is never wrong, only spelled
// differently from the usual configuration.
func importedClusterTypeRef(ctx context.Context, meta *Meta, region string, returned types.String) types.String {
	ref := returned.ValueString()
	typeID, versionID := refTypeAndVersion(ref)
	if meta == nil || meta.Client == nil || versionID == "" || typeID == "" {
		return returned
	}
	var ignored diag.Diagnostics
	versions, ok := meta.listVersions(ctx, region, typeID, &ignored)
	if !ok {
		tflog.Debug(ctx, "kept the pinned cluster type on import: its versions weren't readable",
			map[string]any{"cluster_type": ref})
		return returned
	}
	if newestAvailable(versions) != versionID {
		return returned
	}
	head, _, _ := strings.Cut(ref, versionsSegment)
	return types.StringValue(head)
}

// newestAvailable is the version id an unversioned reference binds: the
// highest-numbered `available` version, or "" when there's none.
func newestAvailable(versions []k0rdent.ClusterTypeVersion) string {
	best, id := int32(-1), ""
	for _, v := range versions {
		if v.Status != k0rdent.ClusterTypeVersionStatusAvailable || v.Version == nil || v.VersionId == nil {
			continue
		}
		if *v.Version > best {
			best, id = *v.Version, v.VersionId.String()
		}
	}
	return id
}
