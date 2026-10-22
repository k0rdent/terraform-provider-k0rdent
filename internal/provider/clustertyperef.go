package provider

import (
	"context"
	"net/url"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
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
	const desc = "replaced only when the new reference names a different ClusterType; another " +
		"version of the same one is upgraded in place or refused"
	return stringplanmodifier.RequiresReplaceIf(func(ctx context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
		clusterTypeRefRequiresReplace(ctx, req, resp)
		if resp.RequiresReplace {
			change, _ := classifyRefChange(req.ConfigValue.ValueString(), req.StateValue.ValueString(), "")
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
