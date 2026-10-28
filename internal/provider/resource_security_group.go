package provider

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ resource.Resource                   = (*securityGroupResource)(nil)
	_ resource.ResourceWithConfigure      = (*securityGroupResource)(nil)
	_ resource.ResourceWithImportState    = (*securityGroupResource)(nil)
	_ resource.ResourceWithIdentity       = (*securityGroupResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*securityGroupResource)(nil)
	_ resource.ResourceWithValidateConfig = (*securityGroupResource)(nil)
	_ resource.ResourceWithUpgradeState   = (*securityGroupResource)(nil)
)

const securityGroupResourceSuffix = "security_group"

func newSecurityGroupResource() resource.Resource { return &securityGroupResource{} }

type securityGroupResource struct {
	meta *Meta
}

type securityGroupModel struct {
	ID          types.String   `tfsdk:"id"`
	Region      types.String   `tfsdk:"region"`
	Project     types.String   `tfsdk:"project"`
	DisplayName types.String   `tfsdk:"display_name"`
	Description types.String   `tfsdk:"description"`
	Tags        types.Map      `tfsdk:"tags"`
	TagsAll     types.Map      `tfsdk:"tags_all"`
	Ingress     []ruleModel    `tfsdk:"ingress"`
	Egress      []ruleModel    `tfsdk:"egress"`
	RuleCount   types.Int64    `tfsdk:"rule_count"`
	OwnerKind   types.String   `tfsdk:"owner_kind"`
	State       types.String   `tfsdk:"state"`
	SelfLink    types.String   `tfsdk:"self_link"`
	Timeouts    timeouts.Value `tfsdk:"timeouts"`

	// uid is the platform's uid for the security group read, which own_object.go
	// keeps in private state. Not an attribute.
	uid string
}

func (m securityGroupModel) metadata() tenantMetadata {
	return tenantMetadata{
		DisplayName: m.DisplayName,
		Description: m.Description,
		Tags:        m.Tags,
		TagsAll:     m.TagsAll,
	}
}

type ruleModel struct {
	Name                 types.String `tfsdk:"name"`
	Action               types.String `tfsdk:"action"`
	Protocol             types.String `tfsdk:"protocol"`
	SourcePrefix         types.String `tfsdk:"source_prefix"`
	DestinationPrefix    types.String `tfsdk:"destination_prefix"`
	SourcePortRange      types.String `tfsdk:"source_port_range"`
	DestinationPortRange types.String `tfsdk:"destination_port_range"`
}

func (r *securityGroupResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(securityGroupResourceSuffix)
}

// UpgradeState migrates a state written at an earlier schema version. See
// state_history.go.
func (r *securityGroupResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return stateHistories[securityGroupResourceSuffix].upgraders()
}

// The two rule lists' descriptions. The data source repeats them.
const (
	ingressRulesDescription = "Rules filtering traffic arriving at the instance, in evaluation order."
	egressRulesDescription  = "Rules filtering traffic leaving the instance, in evaluation " +
		"order.\n\nRules are stateless, with one exception the platform applies on the " +
		"`nico` fabric: an egress `tcp` or `udp` rule with a `destination_port_range` " +
		"and no `source_port_range` is made stateful, so return traffic does not need " +
		"a matching ingress rule. It is a property of the enforcement and is not " +
		"reported back, so nothing in this schema can show it."
)

// ruleAttributes is the shape of one rule, shared by both directions.
//
// Direction is the list a rule sits in, not a field on the rule, which is how
// the contract models it. Flattening both lists into one with a `direction`
// attribute would be the familiar AWS shape and it would lose the thing that
// matters most here: evaluation is first-match in list order, so the two lists
// are ordered independently and merging them makes that order unrepresentable.
func ruleAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"action": schema.StringAttribute{
			Required: true,
			MarkdownDescription: enumProse(k0rdent.SecurityGroupRuleActionValues()) +
				". Lowercase: an uppercase spelling is rejected, not normalised.",
			Validators: []validator.String{oneOfEnum(k0rdent.SecurityGroupRuleActionValues())},
		},
		"protocol": schema.StringAttribute{
			Required: true,
			MarkdownDescription: enumProse(k0rdent.SecurityGroupRuleProtocolValues()) + ". `icmp6` is distinct from " +
				"`icmp`: a rule written for one does not match the other. Only `tcp` and `udp` " +
				"may carry ports.",
			Validators: []validator.String{oneOfEnum(k0rdent.SecurityGroupRuleProtocolValues())},
		},
		"source_prefix": schema.StringAttribute{
			Required: true,
			MarkdownDescription: "Source range in CIDR notation, with host bits zero. Both prefixes " +
				"are required: a rule that constrains one side is written with `0.0.0.0/0` or " +
				"`::/0` on the other, so the intent is in the rule rather than in an omission.",
			Validators: []validator.String{cidrHostBitsZero{}},
		},
		"destination_prefix": schema.StringAttribute{
			Required:            true,
			MarkdownDescription: "Destination range in CIDR notation. See `source_prefix`.",
			Validators:          []validator.String{cidrHostBitsZero{}},
		},
		"source_port_range": schema.StringAttribute{
			Optional: true,
			MarkdownDescription: "A single port (`\"80\"`) or an inclusive range (`\"80-81\"`), as a " +
				"string. Omit it to match any source port. Valid only on `tcp` and `udp`.",
			Validators: []validator.String{portRange{}},
		},
		"destination_port_range": schema.StringAttribute{
			Optional:            true,
			MarkdownDescription: "As `source_port_range`, for the destination side.",
			Validators:          []validator.String{portRange{}},
		},
		"name": schema.StringAttribute{
			Optional: true,
			MarkdownDescription: fmt.Sprintf("A label for the rule. Free text, not unique, and not used for "+
				"ordering: position in the list is the only ordering signal. At most %d characters.",
				k0rdent.SecurityGroupRuleNameMaxLength),
			Validators: []validator.String{stringvalidator.LengthAtMost(k0rdent.SecurityGroupRuleNameMaxLength)},
		},
	}
}

// Schema declares the resource.
//
// # There is no rule resource, and that is deliberate
//
// A rule has no id and no subresource: the group's `rules` object is the
// desired state and an update replaces it wholesale. A separate rule resource
// would have to read-modify-write the whole group for each entry, which races
// with itself the moment two rules are added in the same apply. AWS shipped
// both shapes and the standing advice is to never mix them; there is only one
// shape here.
//
// # Ports are validated in the plan, not by the server
//
// `sourcePortRange` on an `icmp` rule is a 422. The server is right to reject
// it, but the failure lands halfway through an apply that has already been
// approved, so ValidateConfig catches it first.
func (r *securityGroupResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version: stateHistories[securityGroupResourceSuffix].version(),
		MarkdownDescription: "A named, ordered set of CIDR filter rules, applied to the instances of " +
			"whatever it is attached to.\n\n" +
			"This is the platform's nearest thing to an address allowlist. It filters node " +
			"interfaces, not the Kubernetes API endpoint, and there is no allowlist for that " +
			"endpoint on the public API today.\n\n" +
			"Rules are evaluated first-match in list order, and ingress and egress are " +
			"independent. Only a VPC on the `nico` fabric backend can carry security groups.\n\n" +
			"The platform adds no rules to a group. On the `nico` fabric, traffic that no rule " +
			"permits is denied, in both directions. Rules the operator sets for the site are " +
			"checked before the group's, so a site permit or deny wins over the group. Replies to " +
			"a connection a node opened through a stateful egress rule are let back in. A change " +
			"can take up to a few minutes to reach the nodes, and the cluster's Kubernetes API " +
			"address isn't filtered at all.\n\n" +
			"Requires `region` and `project` on the provider.",
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{
				Create: true, Update: true, Delete: true,
			}),
			// Blocks, not attributes, and ordered. `ingress { ... }` repeated is
			// how every provider in the field writes a rule list, and the order
			// the configuration is written in is the order traffic is matched
			// in, so a Set would silently reorder the filter.
			"ingress": schema.ListNestedBlock{
				MarkdownDescription: ingressRulesDescription,
				NestedObject:        schema.NestedBlockObject{Attributes: ruleAttributes()},
			},
			// Ordered for the same reason as ingress, and independently of it:
			// the two lists are evaluated separately.
			"egress": schema.ListNestedBlock{
				MarkdownDescription: egressRulesDescription,
				NestedObject:        schema.NestedBlockObject{Attributes: ruleAttributes()},
			},
		},
		Attributes: map[string]schema.Attribute{
			"region":  regionAttribute(),
			"project": projectAttribute(),
			// Optional+Computed: the provider generates a name when one is
			// omitted, and the name is immutable. Naming the create is what
			// makes a retried one safe: a repeat is a 409 rather than a second
			// group, and there is no idempotency key header.
			"id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Group name, unique in the project among live groups. Omit it " +
					"and the provider generates one, `tf-` and 16 hex digits, so that a create whose " +
					"answer is lost can be read back rather than sent twice.\n\n" +
					"A group mid-teardown keeps its name: recreating one with the same `id` is " +
					"refused with `409` until the old one has finished deleting.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
				Validators: idValidators(),
			},
			"display_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Human-friendly name. Free text, not unique.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form text describing it, for people to read.",
			},
			"tags": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "User-defined labels. The contract types tag values as freeform " +
					"JSON; this schema accepts strings, because a Terraform map has one element " +
					"type and a string is the only one every value round-trips through.",
				Validators: []validator.Map{reservedTagKey{}},
			},
			tagsAllAttribute: tagsAllSchemaAttribute(""),
			"rule_count": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Rules in the group, ingress plus egress, as the platform counts " +
					"them. A write is refused when it would push this above 256.",
				PlanModifiers: []planmodifier.Int64{ruleCount{}},
			},
			// Who owns a group is settled when it's made.
			"owner_kind": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Set to `vpc` when the platform owns this group rather than the " +
					"tenant: a VPC's DEFAULT group. Such a group cannot be modified or deleted, " +
					"so a value here means this resource cannot manage what it is pointed at.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle state as last read from the platform.",
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this group.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
	describeReplacement(&resp.Schema, "security group")
}

// ValidateConfig rejects a port range on a protocol that cannot carry one.
func (r *securityGroupResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	for _, direction := range []string{"ingress", "egress"} {
		// A `dynamic` block over a variable is unknown under `terraform validate`.
		var list types.List
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root(direction), &list)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if list.IsUnknown() {
			continue
		}
		var rules []ruleModel
		resp.Diagnostics.Append(list.ElementsAs(ctx, &rules, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		for i, rule := range rules {
			protocol := rule.Protocol.ValueString()
			if protocol == "" || protocol == "tcp" || protocol == "udp" {
				continue
			}
			for _, port := range []struct {
				attr  string
				value types.String
			}{
				{"source_port_range", rule.SourcePortRange},
				{"destination_port_range", rule.DestinationPortRange},
			} {
				if port.value.IsNull() || port.value.IsUnknown() {
					continue
				}
				// Local: configuration validation, no request made. The server
				// answers 422 for this; saying it here means the apply is never
				// started rather than failing part-done.
				resp.Diagnostics.AddAttributeError(
					path.Root(direction).AtListIndex(i).AtName(port.attr),
					"Ports cannot be set on this protocol",
					fmt.Sprintf("`%s` is set on a rule whose protocol is %q. Only `tcp` and `udp` "+
						"carry ports; the platform rejects the rest rather than ignoring the "+
						"range, because a rule that reads as port-scoped but is not would be a "+
						"security surprise.", port.attr, protocol),
				)
			}
		}
	}
}

func (r *securityGroupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (r *securityGroupResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	modifyScopePlan(ctx, r.meta, req, resp)
	planTagsAll(ctx, r.meta, req, resp)
	// "vpc" specifically: the attribute is populated for every group the
	// platform owns, and `vpc` is the only owner kind the contract declares.
	refusePlatformOwned(ctx, "owner_kind", string(k0rdent.SecurityGroupOwnerKindVpc), req, resp)
	if resp.Diagnostics.HasError() {
		return
	}

	// It compares resp.Plan, not req.Plan: a change to the provider's
	// default_tags is in tags_all alone, and it's sent like any other. The
	// platform runs the group through `updating` to apply a change, and the
	// update waits for it to come back.
	planResume(ctx, req, resp)
	planSettledState(ctx, securityGroupStates.settle, req, resp)
}

func (r *securityGroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan securityGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, plan.Region, plan.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Create, securityGroupStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	md, diags := plan.metadata().create(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	key, err := newCreateKey(plan.ID, true)
	if err != nil {
		addCreateKeyError(&resp.Diagnostics, "security group", err)
		return
	}

	rules := rulesRequest(plan)
	body := k0rdent.SecurityGroupCreateRequest{
		Id:          &key.id,
		DisplayName: md.DisplayName,
		Description: md.Description,
		Tags:        key.taggedFreeform(freeformTags(md.Tags)),
		Rules:       &rules,
	}
	create := convergingCreate{
		key: key, kind: "security group", what: "create the security group",
		resourceName: ResourceName(securityGroupResourceSuffix),
		importID:     scopedImportID(target),
		send: func(ctx context.Context) createAnswer {
			created, err := r.meta.Client.CreateSecurityGroupWithResponse(ctx, target.region, target.project, body)
			if err != nil {
				return createAnswer{err: err}
			}
			a := createAnswer{resp: created.HTTPResponse, body: created.Body}
			if created.JSON201 != nil {
				a.id = created.JSON201.Id
			}
			return a
		},
		lookup: func(ctx context.Context, id string) createLookup {
			got, err := r.meta.Client.GetSecurityGroupWithResponse(ctx, target.region, target.project, id)
			if err != nil {
				return createLookup{err: err}
			}
			return lookupFrom(got.HTTPResponse, got.Body, got.JSON200 != nil,
				func() string { return createTokenOfFreeform(presentMap(got.JSON200.Tags)) })
		},
		timeoutID: timeoutMetadataID("securityGroupId"),
		// A group still tearing down holds its id, so a replacement under a
		// configured id waits for the old one to go rather than failing.
		busy: newBusyRetry("createSecurityGroup", r.meta.Wait, dependsOn(fmt.Sprintf("the security group %q", key.id),
			r.reader(target, key.id), securityGroupStates.state, string(k0rdent.SecurityGroupStateDeleting))),
	}
	created, found := createFinish[securityGroupModel]{
		operation: "createSecurityGroup", target: target,
		wait: func(ctx context.Context, id string) error {
			return r.waitForSettled(ctx, target, id, "the security group to be applied")
		},
		read: func(ctx context.Context, id string) (securityGroupModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, plan)
		},
	}.run(ctx, create, req.Plan, &resp.State, &resp.Diagnostics)
	if found {
		resp.Diagnostics.Append(recordUID(ctx, resp.Private, created.uid)...)
	}
}

func (r *securityGroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State, &req.State)
	var prior securityGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, prior.Region, prior.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	state, found, diags := r.read(ctx, target, prior.ID.ValueString(), prior)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if !refreshedOwnObject(ctx, req.Private, resp, "security group", prior.ID.ValueString(), state.uid) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *securityGroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan, state securityGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	id := state.ID.ValueString()
	if (resumable[securityGroupModel]{
		kind: "security group", resourceName: ResourceName(securityGroupResourceSuffix),
		timeout: plan.Timeouts.Create, fallback: securityGroupStates.timeout(r.meta.Wait),
		wait: func(ctx context.Context) error {
			return r.waitForSettled(ctx, target, id, "the security group to be applied")
		},
		read: func(ctx context.Context) (securityGroupModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, plan)
		},
	}).resume(ctx, req, resp) {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Update, r.meta.Wait.Timeout, &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	md, metadataChanged, diags := plan.metadata().patch(ctx, state.metadata())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// `rules` is desired state, not a patch, and it is always sent with any
	// change: omitting it leaves the platform's current set in place, which
	// is the one thing a Terraform update must never do. An update that
	// changes neither the rules nor the metadata sends nothing. That's the
	// update a default_tags unknown at plan makes, when the apply finds
	// tags_all the same as before.
	rules := rulesRequest(plan)
	if metadataChanged || !reflect.DeepEqual(rules, rulesRequest(state)) {
		if !r.sendUpdate(ctx, target, id, md, rules, &resp.Diagnostics) {
			return
		}
	}

	next, found, diags, err := settledRead(ctx, plan.State, securityGroupStates.settle,
		func(ctx context.Context) (securityGroupModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, plan)
		},
		func(m securityGroupModel) types.String { return m.State },
		func(ctx context.Context) error {
			return r.waitForSettled(ctx, target, id, "the security group change to be applied")
		})
	resp.Diagnostics.Append(diags...)
	if err != nil {
		addWaitError(&resp.Diagnostics, "The security group did not settle after the change", err)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the provider's conclusion from two calls, as in createFinish.
		resp.Diagnostics.AddError(
			"The security group disappeared during the update",
			fmt.Sprintf("Reading %q after the change answered 404.", id),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

// sendUpdate sends the security group's PATCH, the whole desired state, and
// waits for it to be applied. It reports whether that worked.
func (r *securityGroupResource) sendUpdate(ctx context.Context, target clusterTarget, id string, md metadataFields,
	rules k0rdent.SecurityGroupRules, diags *diag.Diagnostics) bool {
	// A group still applying an earlier change refuses this one, which a run
	// whose wait timed out leaves behind. The PATCH carries the whole desired
	// state, so sending it again once the group settles asks for the same.
	busy := newBusyRetry("updateSecurityGroup", r.meta.Wait, dependsOn(fmt.Sprintf("the security group %q", id),
		r.reader(target, id), securityGroupStates.state,
		string(k0rdent.SecurityGroupStateCreating), string(k0rdent.SecurityGroupStateUpdating)))
	var updated *k0rdent.UpdateSecurityGroupResponse
	var err error
	waitErr := busy.sendUntilSettled(ctx, func(ctx context.Context) (*http.Response, []byte, error) {
		updated, err = r.meta.Client.UpdateSecurityGroupWithResponse(ctx, target.region, target.project, id,
			k0rdent.SecurityGroupUpdateRequest{
				DisplayName: md.DisplayName,
				Description: md.Description,
				Tags:        freeformTags(md.Tags),
				Rules:       &rules,
			})
		if err != nil {
			return nil, nil, err
		}
		return updated.HTTPResponse, updated.Body, nil
	})
	if waitErr != nil {
		busy.report(diags, "update the security group", updated.HTTPResponse, updated.Body, waitErr)
		return false
	}
	if err != nil || updated.JSON200 == nil {
		var respBody []byte
		var httpResp *http.Response
		if updated != nil {
			respBody, httpResp = updated.Body, updated.HTTPResponse
		}
		addAPIError(diags, "update the security group", httpResp, respBody, err)
		return false
	}

	if err := r.waitForSettled(ctx, target, id, "the security group change to be applied"); err != nil {
		addWaitError(diags, "The security group did not settle after the change", err)
		return false
	}
	return true
}

func (r *securityGroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state securityGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if forgetPlatformOwned(ctx, "owner_kind", string(k0rdent.SecurityGroupOwnerKindVpc),
		req.State, &resp.Diagnostics) {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, state.Timeouts.Delete, securityGroupStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	send := func(ctx context.Context) (*http.Response, []byte, error) {
		deleted, err := r.meta.Client.DeleteSecurityGroupWithResponse(ctx, target.region, target.project, id)
		if err != nil {
			return nil, nil, err
		}
		return deleted.HTTPResponse, deleted.Body, nil
	}
	// The delete is waited out rather than assumed: a group at `deleting`
	// still holds its name, so returning early would let a replacement in the
	// same apply hit a 409 on a name Terraform believes is free.
	if securityGroupStates.destroy(ctx, r.meta.Wait, send, r.reader(target, id), &resp.Diagnostics) == deleteRefused {
		r.nameTheHolders(ctx, target, id, &resp.Diagnostics)
	}
}

// nameTheHolders turns "in use" into a list of what is using it.
//
// A 409 on this delete means something still holds the group, and the upstream
// message does not say what. Terraform's own graph is no help either: the
// holder is usually a VPC binding or a cluster in another part of the
// configuration, or something not managed here at all. The answer is one extra
// read, and it is only made on the failure path.
//
// Reported as a warning appended to the error, not as the error: the delete
// failed for the reason the platform gave, and this is the detail that makes it
// actionable. A failure to read the attachments must not replace that.
func (r *securityGroupResource) nameTheHolders(ctx context.Context, target clusterTarget, id string, diags *diag.Diagnostics) {
	got, err := r.meta.Client.ListSecurityGroupAttachmentsWithResponse(ctx, target.region, target.project, id)
	if err != nil || got.JSON200 == nil {
		return
	}

	holders := make([]string, 0, len(got.JSON200.Attachments))
	heldByFailed := false
	for _, a := range got.JSON200.Attachments {
		holders = append(holders, fmt.Sprintf("%s %s (%s)", a.Kind, a.Id, a.State))
		heldByFailed = heldByFailed || a.State == string(k0rdent.ClusterStateFailed)
	}
	if len(holders) == 0 {
		// The delete was refused and nothing live holds it, which is worth
		// saying: the blocker is something this read excludes, such as a holder
		// already tearing down.
		diags.AddWarning(
			"Nothing live is holding this security group",
			"The platform refused the delete, but the attachment list is empty. Only live holders "+
				"are listed, so a VPC or cluster that is still tearing down would not appear here. "+
				"Try again once it has finished.",
		)
		return
	}

	detail := fmt.Sprintf("Detach it from these before deleting it:\n\n  %s", strings.Join(holders, "\n  "))
	if heldByFailed {
		// A delete the platform failed leaves the cluster holding the group,
		// and nothing can detach it from a failed cluster.
		detail += "\n\nA `failed` holder is usually a delete the platform couldn't finish. Nothing can " +
			"detach the group from it: the platform releases the group when it finishes deleting the " +
			"holder, so report the holder to whoever runs the landscape. " +
			removedBlockAdvice("security group", ResourceName(securityGroupResourceSuffix))
	}
	diags.AddWarning("What is holding this security group", redactDiagnosticText(detail))
}

func (r *securityGroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importScoped(ctx, r.meta, importForm{
		resource: ResourceName(securityGroupResourceSuffix),
		key:      "id",
	}, req, resp)
}

// IdentitySchema is what an import block's `identity` takes.
func (r *securityGroupResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = scopedIdentitySchema("id", "")
}

func (r *securityGroupResource) read(ctx context.Context, target clusterTarget, id string, prior securityGroupModel) (securityGroupModel, bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	got, err := r.meta.Client.GetSecurityGroupWithResponse(ctx, target.region, target.project, id)
	if err != nil {
		addAPIError(&diags, "read the security group", nil, nil, err)
		return securityGroupModel{}, false, diags
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		return securityGroupModel{}, false, diags
	}
	if got.JSON200 == nil {
		addAPIError(&diags, "read the security group", got.HTTPResponse, got.Body, nil)
		return securityGroupModel{}, false, diags
	}

	out, d := securityGroupStateFrom(ctx, *got.JSON200, prior, r.meta.defaultTags())
	diags.Append(d...)
	out.uid = uidOf(got.JSON200.Uid)
	return out, true, diags
}

func (r *securityGroupResource) waitForSettled(ctx context.Context, target clusterTarget, id, what string) error {
	return securityGroupStates.awaitSettled(ctx, r.meta.Wait, what, r.reader(target, id))
}

// reader reads the security group for a wait.
func (r *securityGroupResource) reader(target clusterTarget, id string) stateRead[k0rdent.SecurityGroup] {
	return func(ctx context.Context) (*k0rdent.SecurityGroup, *http.Response, []byte, error) {
		got, err := r.meta.Client.GetSecurityGroupWithResponse(ctx, target.region, target.project, id)
		if err != nil {
			return nil, nil, nil, err
		}
		return got.JSON200, got.HTTPResponse, got.Body, nil
	}
}

// securityGroupStates is what a security group's waits make of each state it
// reports.
var securityGroupStates = stateKind[k0rdent.SecurityGroup]{
	name:     "security group",
	resource: securityGroupResourceSuffix,
	// The contract answers a repeated delete of a `deleting` group with 204,
	// not 409. Read on a 409 all the same, so that a platform that refuses
	// it instead doesn't fail the provider's own delete.
	deleting: string(k0rdent.SecurityGroupStateDeleting),
	state: func(g k0rdent.SecurityGroup) (string, bool) {
		return stateString(g.State), stateString(g.State) != ""
	},
	// A security group carries no `stateMessage` or `stateReason`, only
	// `statusReason`.
	detail: func(g k0rdent.SecurityGroup) string { return stateDetail(g.StatusReason, "") },
	settle: stateTable{
		missing: stateRule{verdict: settled},
		states: map[string]stateRule{
			string(k0rdent.SecurityGroupStateActive):   {verdict: settled},
			string(k0rdent.SecurityGroupStateCreating): {verdict: keepWaiting},
			string(k0rdent.SecurityGroupStateUpdating): {verdict: keepWaiting},
			string(k0rdent.SecurityGroupStateDeleting): {verdict: keepWaiting},
			string(k0rdent.SecurityGroupStateFailed):   {verdict: failsTerminally, detail: true},
		},
	},
	delete: stateTable{
		missing: stateRule{verdict: keepWaiting},
		states: map[string]stateRule{
			string(k0rdent.SecurityGroupStateDeleting): {verdict: keepWaiting},
			string(k0rdent.SecurityGroupStateFailed):   {verdict: failsTerminally, detail: true},
			string(k0rdent.SecurityGroupStateActive):   {verdict: failsTerminally, says: afterDelete},
			string(k0rdent.SecurityGroupStateCreating): {verdict: failsTerminally, says: afterDelete},
			string(k0rdent.SecurityGroupStateUpdating): {verdict: failsTerminally, says: afterDelete},
		},
	},
}

func rulesRequest(m securityGroupModel) k0rdent.SecurityGroupRules {
	// Both lists are always present, never nil. An omitted `rules` leaves the
	// group's rules alone and empty lists clear them, and a configuration with
	// no blocks means the second.
	out := k0rdent.SecurityGroupRules{
		Ingress: make([]k0rdent.SecurityGroupRule, 0, len(m.Ingress)),
		Egress:  make([]k0rdent.SecurityGroupRule, 0, len(m.Egress)),
	}
	for _, rule := range m.Ingress {
		out.Ingress = append(out.Ingress, ruleRequest(rule))
	}
	for _, rule := range m.Egress {
		out.Egress = append(out.Egress, ruleRequest(rule))
	}
	return out
}

func ruleRequest(rule ruleModel) k0rdent.SecurityGroupRule {
	return k0rdent.SecurityGroupRule{
		Name:                 optionalString(rule.Name),
		Action:               k0rdent.SecurityGroupRuleAction(rule.Action.ValueString()),
		Protocol:             k0rdent.SecurityGroupRuleProtocol(rule.Protocol.ValueString()),
		SourcePrefix:         rule.SourcePrefix.ValueString(),
		DestinationPrefix:    rule.DestinationPrefix.ValueString(),
		SourcePortRange:      optionalString(rule.SourcePortRange),
		DestinationPortRange: optionalString(rule.DestinationPortRange),
	}
}

func securityGroupStateFrom(ctx context.Context, g k0rdent.SecurityGroup, prior securityGroupModel, defaults defaultTags) (securityGroupModel, diag.Diagnostics) {
	tags, diags := tagAnswerOfRequired(g.Tags).forResource(prior.Tags)
	md := prior.metadata().read(metadataAnswer{DisplayName: g.DisplayName, Description: g.Description, Tags: tags, Defaults: defaults})

	out := securityGroupModel{
		ID:          types.StringValue(g.Id),
		Region:      prior.Region,
		Project:     prior.Project,
		Timeouts:    prior.Timeouts,
		DisplayName: md.DisplayName,
		Description: md.Description,
		Tags:        md.Tags,
		TagsAll:     md.TagsAll,
		RuleCount:   int64OrNull(g.RuleCount),
		OwnerKind:   types.StringNull(),
		State:       types.StringNull(),
		SelfLink:    stringOrNull(g.SelfLink),
	}
	if g.OwnerKind != nil {
		out.OwnerKind = types.StringValue(string(*g.OwnerKind))
	}
	if g.State != nil {
		out.State = types.StringValue(string(*g.State))
	}

	out.Ingress = rulesFrom(g.Rules.Ingress, prior.Ingress)
	out.Egress = rulesFrom(g.Rules.Egress, prior.Egress)

	return out, diags
}

// rulesFrom reads a rule list back by position. The platform stores "80-80" as
// "80", so a configured spelling it treats as the same port is kept.
func rulesFrom(rules []k0rdent.SecurityGroupRule, prior []ruleModel) []ruleModel {
	var out []ruleModel
	for i, rule := range rules {
		m := ruleFrom(rule)
		if i < len(prior) {
			m.SourcePortRange = keepPortSpelling(prior[i].SourcePortRange, m.SourcePortRange)
			m.DestinationPortRange = keepPortSpelling(prior[i].DestinationPortRange, m.DestinationPortRange)
		}
		out = append(out, m)
	}
	return out
}

func keepPortSpelling(prior, got types.String) types.String {
	if prior.IsNull() || prior.IsUnknown() || got.IsNull() {
		return got
	}
	if lo, hi, ok := strings.Cut(prior.ValueString(), "-"); ok && lo == hi && lo == got.ValueString() {
		return prior
	}
	return got
}

func ruleFrom(rule k0rdent.SecurityGroupRule) ruleModel {
	return ruleModel{
		Name:                 stringOrNull(rule.Name),
		Action:               types.StringValue(string(rule.Action)),
		Protocol:             types.StringValue(string(rule.Protocol)),
		SourcePrefix:         types.StringValue(rule.SourcePrefix),
		DestinationPrefix:    types.StringValue(rule.DestinationPrefix),
		SourcePortRange:      stringOrNull(rule.SourcePortRange),
		DestinationPortRange: stringOrNull(rule.DestinationPortRange),
	}
}
