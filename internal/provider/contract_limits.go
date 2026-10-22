package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The contract's limits on values the provider sends, as plan-time checks.
//
// Each one is a 422 at apply time otherwise, after the plan was approved and
// sometimes after other resources in it were already made. AWS and Google
// providers catch these at plan, and so does this one. The numbers and
// patterns are the client's, generated from the contract, so they move with
// it. TestEverySpecConstraintIsCheckedAtPlanTime probes the attribute that
// carries each, so one left without a check fails there, not in a run.

// idMaxLength is the length every id the contract names caps at.
const idMaxLength = 63

var (
	macAddressPattern = regexp.MustCompile(k0rdent.ConsumerNetworkMacAddressPattern)
	guidPattern       = regexp.MustCompile(k0rdent.ConsumerNetworkGuidPattern)
	// mountPathPattern is absolute, with no empty segment and no control
	// character.
	mountPathPattern  = regexp.MustCompile(k0rdent.ConsumerMountPathPattern)
	vlanIDPattern     = regexp.MustCompile(k0rdent.NetworkCreateRequestEVPNVlanIdPattern)
	pkeyPattern       = regexp.MustCompile(k0rdent.NetworkCreateRequestInfiniBandInfinibandPkeyPattern)
	instanceIDPattern = regexp.MustCompile(k0rdent.GetInstancePathIdPattern)
	// uuidPattern is the contract's `format: uuid`.
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// idValidators are the checks on an id the contract names: an identifier, as
// urlSafeIdentifier says, and at most 63 characters.
func idValidators() []validator.String {
	return []validator.String{urlSafeIdentifier{}, stringvalidator.LengthAtMost(idMaxLength)}
}

// versionID refuses a ClusterType version id that isn't a UUID, the
// contract's `format: uuid`, with the error Read gave before this check moved
// to the plan.
type versionID struct{}

func (versionID) Description(context.Context) string {
	return "must be a ClusterType version id, which is a UUID"
}

func (v versionID) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (versionID) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() || uuidPattern.MatchString(req.ConfigValue.ValueString()) {
		return
	}
	// Local: configuration validation, no request made.
	resp.Diagnostics.AddAttributeError(req.Path, "Not a version id",
		fmt.Sprintf("%q is not a UUID. Version ids come from `%s`.",
			req.ConfigValue.ValueString(), ResourceName(clusterTypeVersionsDataSourceSuffix)))
}

// clusterTypeVersionRef refuses a ClusterType reference whose version part
// isn't a UUID.
//
// A reference is a self-link, which may name a version:
// `.../cluster-types/<id>/versions/<uuid>`. The create sends the reference as
// it is, but an upgrade sends the version on its own, as the contract's
// `clusterTypeVersionId`, which is a UUID. A version that isn't one would be
// taken at create and refused at the first upgrade, so it is refused here.
type clusterTypeVersionRef struct{}

func (clusterTypeVersionRef) Description(context.Context) string {
	return "must be a ClusterType reference whose version, when it names one, is a UUID"
}

func (v clusterTypeVersionRef) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (clusterTypeVersionRef) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	ref := req.ConfigValue.ValueString()
	// Read from the value as written, not as refPath tidies it, so that a
	// version with anything around it, or a `versions` with none after it, is
	// refused rather than quietly read as no version at all.
	const typesSegment = "/cluster-types/"
	at := strings.LastIndex(ref, typesSegment)
	if at < 0 {
		return
	}
	_, after, _ := strings.Cut(ref[at+len(typesSegment):], "/")
	versionsWord := strings.Trim(versionsSegment, "/")
	if after != versionsWord && !strings.HasPrefix(after, versionsWord+"/") {
		return
	}
	version := strings.TrimPrefix(strings.TrimPrefix(after, versionsWord), "/")
	if uuidPattern.MatchString(version) {
		return
	}
	// Local: configuration validation, no request made.
	resp.Diagnostics.AddAttributeError(req.Path, "Not a ClusterType version",
		fmt.Sprintf("%q names the version %q, which isn't a UUID. A version is named by the "+
			"`version_id` the cluster type versions data source reports, as "+
			"`.../cluster-types/<id>/versions/<version_id>`.", ref, version))
}

// The framework's composed validators describe themselves in a grammar nobody
// would write: "map must contain at least 1 elements", "element value must
// satisfy all validations: a + b". The registry page shows each validator's
// own description (schema_docs.go), so these keep the framework's check and
// give it a sentence written for the reader.
type describedList struct {
	validator.List
	text string
}

func (d describedList) Description(context.Context) string         { return d.text }
func (d describedList) MarkdownDescription(context.Context) string { return d.text }

type describedMap struct {
	validator.Map
	text string
}

func (d describedMap) Description(context.Context) string         { return d.text }
func (d describedMap) MarkdownDescription(context.Context) string { return d.text }

// idRule is what idValidators accept, as one clause.
var idRule = fmt.Sprintf("must be lower-case letters, digits and hyphens, starting with a letter, "+
	"not ending with a hyphen, and at most %d characters long", idMaxLength)

// poolKeys checks a node_pool map's keys, which are ClusterType pool ids.
func poolKeys() validator.Map {
	return describedMap{mapvalidator.KeysAre(idValidators()...), "each pool id " + idRule}
}

// idList checks every element of a list of ids.
func idList(what string) validator.List {
	return describedList{listvalidator.ValueStringsAre(idValidators()...), "each " + what + " " + idRule}
}
