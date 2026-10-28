package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ resource.Resource                 = (*clusterResource)(nil)
	_ resource.ResourceWithConfigure    = (*clusterResource)(nil)
	_ resource.ResourceWithImportState  = (*clusterResource)(nil)
	_ resource.ResourceWithIdentity     = (*clusterResource)(nil)
	_ resource.ResourceWithModifyPlan   = (*clusterResource)(nil)
	_ resource.ResourceWithUpgradeState = (*clusterResource)(nil)
	_ resource.ResourceWithMoveState    = (*clusterResource)(nil)
)

// clusterResourceSuffix is this resource's half of its Terraform name. The
// prefix comes from ProviderTypeName, so a rename does not reach this file.
const clusterResourceSuffix = "kubernetes_cluster"

func newClusterResource() resource.Resource {
	return guardReplacements(&clusterResource{}, clusterTenantState)
}

type clusterResource struct {
	meta *Meta
}

// clusterModel is the Terraform state shape, not an API type. Nothing here is
// serialised to the platform: every request and response body comes from the
// generated types, so a hand-written struct cannot drift from the contract.
type clusterModel struct {
	// OnDestroyBusy is what a destroy does with a cluster mid-operation. It is
	// never sent to the platform: see cluster_destroy_busy.go.
	OnDestroyBusy types.String `tfsdk:"on_destroy_busy"`

	ID                  types.String   `tfsdk:"id"`
	Region              types.String   `tfsdk:"region"`
	Project             types.String   `tfsdk:"project"`
	ClusterType         types.String   `tfsdk:"cluster_type"`
	ClusterTypeResolved types.String   `tfsdk:"cluster_type_resolved"`
	KubernetesVersion   types.String   `tfsdk:"kubernetes_version"`
	DisplayName         types.String   `tfsdk:"display_name"`
	Description         types.String   `tfsdk:"description"`
	Tags                types.Map      `tfsdk:"tags"`
	TagsAll             types.Map      `tfsdk:"tags_all"`
	NodePool            types.Map      `tfsdk:"node_pool"`
	NodeCount           types.Int64    `tfsdk:"node_count"`
	State               types.String   `tfsdk:"state"`
	SelfLink            types.String   `tfsdk:"self_link"`
	VPC                 types.String   `tfsdk:"vpc"`
	SecurityGroups      types.List     `tfsdk:"security_groups"`
	StorageCeilingGiB   types.Int64    `tfsdk:"storage_ceiling_gib"`
	SSHAccess           types.Set      `tfsdk:"ssh_access"`
	Timeouts            timeouts.Value `tfsdk:"timeouts"`

	// uid is the platform's uid for the cluster read, which own_object.go
	// keeps in private state. Not an attribute.
	uid string
}

func (m clusterModel) metadata() tenantMetadata {
	return tenantMetadata{
		DisplayName: m.DisplayName,
		Description: m.Description,
		Tags:        m.Tags,
		TagsAll:     m.TagsAll,
	}
}

// clusterNodePoolModel is one entry of the `node_pool` map.
type clusterNodePoolModel struct {
	NodeCount types.Int64 `tfsdk:"node_count"`
}

func clusterNodePoolObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"node_count": types.Int64Type,
	}}
}

func (r *clusterResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(clusterResourceSuffix)
}

// UpgradeState migrates a state written at an earlier schema version. See
// state_history.go.
func (r *clusterResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return stateHistories[clusterResourceSuffix].upgraders()
}

// MoveState accepts a `moved` block from the type this resource was
// registered under before the provider name settled.
func (r *clusterResource) MoveState(context.Context) []resource.StateMover {
	return stateHistories[clusterResourceSuffix].movers()
}

// Schema declares the resource.
//
// # What is deliberately not here
//
// The surface is smaller than the API's, and each omission is a decision rather
// than an oversight. The per-attribute hazard analysis behind these is in
// `wave2-noop-drift-checklist.md`; the short reasons:
//
//   - `audit_policy`: write-only. `Cluster` does not echo it, so it can't be
//     refreshed or drift-detected. `ssh_access` isn't echoed either, but the
//     platform reports it at a path of its own: see cluster_ssh_access.go.
//   - a configurable `kubernetes_version`: the contract says the request field
//     is "accepted for backwards compatibility … but ignored". An attribute
//     that silently does nothing is the failure this provider already avoided
//     once with `organization`. The computed `kubernetes_version` here is a
//     reading of the bound version, not that field, and sends nothing.
//   - `health`: reverts to `unknown` at read time once a staleness boundary
//     passes, with no user action and no server event. A computed attribute
//     carrying it makes plans go noisy on their own.
//   - `updated_at`, `state_message`, `state_reason`, `runtime`,
//     `next_available_cluster_type_version`: each moves, or is populated on
//     some operations and not others, without a user write. Storing any of them
//     turns a refresh into a diff.
func (r *clusterResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version: stateHistories[clusterResourceSuffix].version(),
		MarkdownDescription: "A Kubernetes cluster, created from a ClusterType in a project.\n\n" +
			"Requires `region` and `project`, on the provider block or on this resource. " +
			"Create, scale and destroy are asynchronous on the platform; this resource waits for " +
			"each to settle, bounded by its `timeouts` block. Create and delete default to `" +
			shortDuration(DefaultLongOperationTimeout) + "`, the rest to the provider's `operation_timeout`.\n\n" +
			"A wait that times out, or a run that is interrupted, does not stop the operation, which keeps " +
			"running on the platform. A create that stops waiting fails, and Terraform taints the cluster. " +
			"Run `terraform untaint` on it and then `terraform apply`, which waits for the same cluster. " +
			"A plan that would replace it instead warns before the approval, in whatever state the " +
			"platform reports, that applying it cancels what's still being built, or deletes the " +
			"cluster, and creates it again. `terraform destroy` runs that plan first to refresh state, so " +
			"it shows the warning too, and then only deletes. A failed cluster's delete can fail too. " +
			"A create of an `id` the platform holds for a cluster without this provider's create token, " +
			"such as one made outside Terraform, is refused unless `allow_replace = true`. One with the " +
			"token passes, since only an apply can tell whether this state holds it: if another state " +
			"does, the create fails with \"already exists\" and nothing is deleted. " +
			"A destroy that stops waiting leaves the cluster in state, and the next `terraform destroy` " +
			"waits for the same delete without sending another.\n\n" +
			"The platform deletes a cluster only once nothing is running on it, so destroying one that " +
			"is still `creating` or `updating` " +
			"cancels that operation first and warns that it did, all within the delete timeout. A plan " +
			"that destroys the cluster, or replaces one that isn't tainted, warns before the approval too " +
			"when the last refresh saw the operation, because an operation cancelled that way does not " +
			"finish: a cancelled upgrade leaves the cluster `failed`. A plan that replaces a tainted " +
			"cluster warns from the platform's read, since Terraform doesn't show the provider the " +
			"tainted one. " +
			"Only a destroy cancels an operation, the one that starts an apply replacing the cluster " +
			"included, and `on_destroy_busy = \"wait\"` makes a destroy wait for the operation to finish " +
			"instead. To cancel it and keep the cluster, invoke the `" +
			ResourceName(clusterCancelOperationActionSuffix) + "` action.",
		Blocks: map[string]schema.Block{
			// Create, update and delete each wait on the platform, so each gets
			// its own default, create's and delete's longer. Read does not
			// wait and has none.
			"timeouts": hardwareTimeouts(ctx),
		},
		Attributes: map[string]schema.Attribute{
			"region":  regionAttribute(),
			"project": projectAttribute(),
			"id":      hardwareIDAttribute("Cluster"),
			"cluster_type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Reference to the ClusterType to create this cluster from, as returned in a " +
					"cluster type's `self_link`, for example `" + exampleClusterTypeRef + "`. " +
					"The `/versions/{id}` segment is optional: omit it to bind the ClusterType's " +
					"current version, include it to pin one. Both a path and an absolute URL are " +
					"accepted. Changing it to another ClusterType replaces the cluster.\n\n" +
					"Pinning a later `available` version of the same ClusterType upgrades the cluster " +
					"in place, one version per step: the plan says how many steps it takes, and the " +
					"apply waits out each before the next, skipping any `draft` or `deprecated` " +
					"version between. The update timeout bounds the whole apply. If a step fails, the " +
					"apply stops, state records the version the cluster reached, and nothing is " +
					"deleted; the platform does not retry an upgrade from `failed`.\n\n" +
					"Node count changes in the same apply are made before or after each step, " +
					"wherever the versions' node pool bounds admit them, since a scale is checked " +
					"against the version the cluster is on and an upgrade against the one it moves " +
					"to. The plan fails when no order works, and for an earlier version, one that is " +
					"not `available`, a version the ClusterType does not have, or a BMaaS-only version " +
					"in the way. See `cluster_type_resolved` for what the platform bound." +
					knownGap("A tenant can't read the hardware or the image behind a ClusterType's machine types, "+
						"so choosing a ClusterType for its hardware means writing one an operator named."),
				PlanModifiers: []planmodifier.String{clusterTypeVersionGuard()},
				Validators:    []validator.String{clusterTypeVersionRef{}},
			},
			"cluster_type_resolved": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The `clusterType` value the platform returned, verbatim. " +
					"It may differ from `cluster_type`: the platform may resolve an unversioned " +
					"reference to a pinned `/versions/{id}` one, and it may answer with a path " +
					"where an absolute URL was given. This attribute exists so that resolution is " +
					"visible in state rather than hidden by the provider's comparison.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"kubernetes_version": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The Kubernetes version the bound ClusterType version carries, " +
					"such as `v1.35.1+k0s.0`, read from that version's `k8s.k0sVersion`. It's read-only: " +
					"the platform versions a cluster by its ClusterType version and ignores a declared " +
					"Kubernetes version, so pinning `cluster_type` to a later version is how it changes. " +
					"A plan that upgrades shows the version the upgrade reaches. It's the version the " +
					"cluster is bound to, not a reading of what runs: while an upgrade rolls out, the " +
					"`" + ResourceName(clusterDataSourceSuffix) + "` data source's `control_plane_version` " +
					"is what the control plane runs. Null when the bound version carries no Kubernetes " +
					"configuration, or the platform no longer finds it.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"display_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Human-friendly name. Mutable.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. Mutable.",
			},
			"tags": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "User-defined labels. Mutable.",
				Validators:          []validator.Map{reservedTagKey{}},
			},
			tagsAllAttribute: tagsAllSchemaAttribute(""),
			// Optional+Computed, and the drift cost is real and accepted: a pool
			// this map does not name is not drift-checked, because the platform
			// materializes it from the ClusterType and Terraform has no
			// configured value to compare. Named pools are checked normally.
			//
			// Left out of the configuration, Terraform proposes null rather than
			// the prior value, so without UseStateForUnknown every plan would
			// be an update. A configured map is known and never reaches it.
			"node_pool": schema.MapNestedAttribute{
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Per-pool node counts, keyed by the ClusterType node pool's id.\n\n" +
					"This is a **sparse override set**, not the full pool list. The platform " +
					"materializes one pool for every pool on the bound ClusterType version, using " +
					"each pool's default count where this map does not name it, so naming one pool " +
					"here does not remove the others. A pool named here is managed by Terraform: " +
					"changing its count scales the cluster. A pool left out is left to the " +
					"ClusterType's default and is not drift-checked.\n\n" +
					"Omit the attribute entirely and the platform's materialized counts are read " +
					"into state as-is." +
					knownGap("A pool's machine type is readable only as a name, not as hardware, "+
						"so choosing a pool for its hardware means writing its id as a literal."),
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"node_count": schema.Int64Attribute{
							Required: true,
							MarkdownDescription: "Number of nodes in this pool. Must lie within the ClusterType " +
								"pool's `node_count_min` and `node_count_max` where those are set.",
							Validators: []validator.Int64{int64validator.AtLeast(0)},
						},
					},
				},
				Validators: []validator.Map{poolKeys()},
			},
			// Both keep their state value here so that ModifyPlan can see a
			// no-op, and ModifyPlan then decides what a change can move.
			"node_count": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Total nodes across every materialized pool, as reported by the platform.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Lifecycle state as last read. This resource waits for a settled state, " +
					"so it is normally `active`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this cluster.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"vpc": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Id of the VPC this cluster brought with it. Name the VPC through " +
					"this rather than a data source: it stays known while the cluster changes, so " +
					"a peering or a security-group binding that names it is not replaced. A peering takes it " +
					"as `vpc`, and a peering in this project that points at it takes it as `remote_vpc`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"security_groups":     clusterSecurityGroupsAttribute(),
			"storage_ceiling_gib": storageCeilingAttribute(),
			"ssh_access": schema.SetNestedAttribute{
				Optional: true,
				MarkdownDescription: "Linux users written onto every node when the cluster is provisioned, each " +
					"with the public keys allowed to log in as that user. Up to " +
					fmt.Sprint(k0rdent.ClusterCreateRequestSshAccessMaxItems) + " users. Only public keys go " +
					"here. Keys reach a node as it's provisioned, so SSH answers before the cluster is `active`.\n\n" +
					"A refresh reads the users the platform reports, so an import fills this in and a " +
					"difference from the configuration shows in the plan. Where the platform doesn't report " +
					"them, state keeps what the provider sent.\n\n" +
					"**Changing it replaces the cluster**, because the platform won't change a running cluster's users. " +
					"Leaving it out and `[]` both mean no users, so moving between them replaces nothing. " +
					"Where the platform doesn't report the users, a cluster that was imported, or created by an " +
					"earlier version of the provider, has no record of them: a change is recorded without " +
					"replacing anything, the nodes keep the users they have, and the plan warns. Use `terraform " +
					"apply -replace` to put new users on such a cluster's nodes.",
				Validators: []validator.Set{setvalidator.SizeAtMost(k0rdent.ClusterCreateRequestSshAccessMaxItems)},
				PlanModifiers: []planmodifier.Set{setplanmodifier.RequiresReplaceIf(clusterSSHAccessRequiresReplace,
					"replaced when it changes, unless state has no record of the nodes' users",
					"replaced when it changes, unless state has no record of the nodes' users")},
				NestedObject: sshUserNestedObject(),
			},
			onDestroyBusyAttribute: onDestroyBusySchemaAttribute(),
		},
	}
}

// clusterSecurityGroupsAttribute is the cluster's own security-group binding.
//
// A List and not a Set: the contract says the order "is significant and
// preserved end to end", and a group listed earlier takes precedence over one
// listed later, so reordering two groups is a real change.
//
// Optional and Computed, with UseStateForUnknown, the same shape as
// `node_pool`, so that leaving it out and writing `[]` mean different things,
// as they do on the API ("Sending `[]` detaches everything. Omitting the field
// changes nothing"):
//
//   - Left out, the create sends no `securityGroups`, an update never sends one,
//     and the plan carries whatever the platform reports. A binding made
//     outside Terraform is read into state and left alone, and removing the
//     attribute from a configuration that had set it leaves the groups attached.
//   - Written as a list, it is managed: it goes on the create when it has any
//     entries, a difference from what the platform reports is drift, and a
//     change is one PATCH carrying the field on its own, then a wait.
//   - Written as `[]`, it is managed and empty: the update sends `[]`, which
//     detaches every group, whoever attached it.
//
// The alternative, Optional without Computed, would read an unset attribute
// as "attach nothing" and detach groups this configuration never named on the
// first apply after someone else attached them.
//
// A group deleted outside Terraform does not leave the plan stuck: the
// platform refuses to delete a group that a cluster holds, answering 409
// CONFLICT_IN_USE and naming the cluster.
func clusterSecurityGroupsAttribute() schema.ListAttribute {
	return schema.ListAttribute{
		Optional:    true,
		Computed:    true,
		ElementType: types.StringType,
		PlanModifiers: []planmodifier.List{
			listplanmodifier.UseStateForUnknown(),
		},
		Validators: []validator.List{
			// Each one a plan-time copy of a 422 the platform answers.
			listvalidator.SizeAtMost(k0rdent.ClusterCreateRequestSecurityGroupsMaxItems),
			listvalidator.UniqueValues(),
			describedList{listvalidator.ValueStringsAre(
				stringvalidator.LengthBetween(k0rdent.ClusterCreateRequestSecurityGroupsItemMinLength,
					k0rdent.ClusterCreateRequestSecurityGroupsItemMaxLength),
				stringvalidator.RegexMatches(pathSegment, "must be a security group id: lower-case "+
					"letters, digits and hyphens, starting with a letter and not ending with a hyphen"),
			), fmt.Sprintf("each security group id must be lower-case letters, digits and hyphens, starting "+
				"with a letter, not ending with a hyphen, and from %d to %d characters long",
				k0rdent.ClusterCreateRequestSecurityGroupsItemMinLength,
				k0rdent.ClusterCreateRequestSecurityGroupsItemMaxLength)},
		},
		MarkdownDescription: "Ids of the security groups that filter this cluster's machines, in the " +
			"order the fabric applies them. Order is significant: a group listed earlier takes " +
			"precedence over one listed later, so reordering the list is a change. At most " +
			fmt.Sprint(k0rdent.ClusterCreateRequestSecurityGroupsMaxItems) + ", " +
			"each named once.\n\n" +
			"Changing the list updates the cluster in place: the provider sends the new list on its " +
			"own and waits for the cluster to settle. The cluster is never replaced for it.\n\n" +
			"**Leaving it out and writing `[]` differ.** Left out, the provider sends nothing, never " +
			"detaches anything, and reads whatever groups the platform reports into state without " +
			"treating them as drift. Removing the attribute from a configuration that had set it " +
			"therefore leaves the groups attached. Written as `[]`, the provider detaches every " +
			"group on the cluster, including any attached outside Terraform.\n\n" +
			"The platform answers 422 when an id is unknown or in another project, when it names a " +
			"VPC's platform-managed default group, or when no node pool on the cluster type has an " +
			"interface on a network in a `nico`-backed VPC for the groups to go on. A group a cluster " +
			"holds can't be deleted until it is detached.\n\n" +
			"These groups filter the machines' network interfaces, not the Kubernetes API address.",
	}
}

func (r *clusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan marks the volatile computed attributes unknown when, and only
// when, something that moves them is changing.
//
// Terraform rejects an apply whose result differs from a known planned value
// ("Provider produced inconsistent result after apply"), so a computed attribute
// that can move during an update has to be planned as unknown. Doing that
// unconditionally would break the property this resource exists to
// keep: an unchanged resource would plan as an in-place update with
// `node_count` "known after apply". So the guard is the first thing here: if
// the planned object equals prior state, nothing is happening and nothing is
// marked. Past it, `node_count` goes unknown only for a scale or an upgrade,
// and `state` keeps a settled value (plan_known.go).
func (r *clusterResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Runs on every plan, create included: the resolved scope has to reach state.
	modifyScopePlan(ctx, r.meta, req, resp)
	// Before the no-op check below: a change to the provider's default_tags
	// is a change to this cluster, though nothing in its configuration moved.
	planTagsAll(ctx, r.meta, req, resp)
	if resp.Diagnostics.HasError() {
		return
	}
	warnUnrecordedSSHAccess(ctx, req, resp)

	if !req.State.Raw.IsNull() && (req.Plan.Raw.IsNull() || len(resp.RequiresReplace) > 0) {
		warnOnBusyDestroy(ctx, req.State, &resp.Diagnostics)
	}
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy; the framework already handles both
	}
	// A create an earlier apply stopped waiting for plans an update that
	// waits for it, though nothing in the configuration moved.
	planResume(ctx, req, resp)
	// resp.Plan, not req.Plan: the scope has just been resolved into it.
	if resp.Plan.Raw.Equal(req.State.Raw) {
		return // no-op
	}
	// The total over every pool, the ones the configuration doesn't name
	// included, so only a scale or a new ClusterType version moves it. A
	// rename, new tags or new security groups leave it as it is, and so does
	// `cluster_type` spelling the version already bound, as it does after an
	// import.
	if inputsChanged(ctx, resp.Plan, req.State, []string{"node_pool"}, &resp.Diagnostics) ||
		bindingMoves(ctx, resp.Plan, req.State, &resp.Diagnostics) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("node_count"), types.Int64Unknown())...)
	}
	planSettledState(ctx, clusterStates.settle, req, resp)
	r.planVersionChange(ctx, req, resp)
}

func (r *clusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan clusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, plan.Region, plan.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Create, clusterStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	key, err := newCreateKey(plan.ID, true)
	if err != nil {
		addCreateKeyError(&resp.Diagnostics, "cluster", err)
		return
	}
	body := k0rdent.ClusterCreateRequest{
		Id:          &key.id,
		ClusterType: plan.ClusterType.ValueString(),
	}
	md, diags := plan.metadata().create(ctx)
	resp.Diagnostics.Append(diags...)
	body.DisplayName, body.Description, body.Tags = md.DisplayName, md.Description, key.tagged(md.Tags)

	pools, diags := clusterPoolsFromModel(ctx, plan.NodePool)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(pools) > 0 {
		overrides := make([]k0rdent.ClusterNodePoolCreateRequest, 0, len(pools))
		for _, id := range sortedKeys(pools) {
			count := pools[id]
			overrides = append(overrides, k0rdent.ClusterNodePoolCreateRequest{Id: id, NodeCount: &count})
		}
		body.NodePools = &overrides
	}
	// Only a list with entries goes on the create. Left out, the platform
	// starts the cluster with none, which is also what `[]` asks for, so
	// sending `[]` would say nothing more.
	groups, diags := optionalStringList(ctx, plan.SecurityGroups)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if groups != nil && len(*groups) > 0 {
		body.SecurityGroups = groups
	}
	body.Storage = storageRequest(plan.StorageCeilingGiB)
	body.SshAccess, diags = sshAccessRequest(ctx, plan.SSHAccess)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The create response is NOT the source of state. It is one of several
	// truthful shapes for this resource: `nextAvailableClusterTypeVersion`
	// is populated on reads and never on mutations, and a pool's
	// `targetNodeCount` comes and goes while a change is in flight. Storing a
	// mutation body and then refreshing reports a change nobody made. So every
	// path in this resource takes its state from the single GET, and the create
	// response is used for exactly one thing: learning the id.
	//
	// A 504 whose workflow start timed out names the cluster it persisted in
	// `details[].metadata.clusterId`. That is the service's own answer to this
	// request, so that cluster is recorded and waited out like a 201.
	create := convergingCreate{
		key: key, kind: "cluster", what: "create the cluster",
		resourceName: ResourceName(clusterResourceSuffix),
		importID:     scopedImportID(target),
		send: func(ctx context.Context) createAnswer {
			created, err := r.meta.Client.CreateClusterWithResponse(ctx, target.region, target.project, body)
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
			got, err := r.meta.Client.GetClusterWithResponse(ctx, target.region, target.project, id, nil)
			if err != nil {
				return createLookup{err: err}
			}
			return lookupFrom(got.HTTPResponse, got.Body, got.JSON200 != nil,
				func() string { return createTokenOf(presentMap(got.JSON200.Tags)) })
		},
		timeoutID: timeoutMetadataID("clusterId"),
	}
	created, found := createFinish[clusterModel]{
		operation: "createCluster", target: target,
		wait: func(ctx context.Context, id string) error {
			return r.waitForSettled(ctx, target, id, "the cluster to finish creating")
		},
		read: func(ctx context.Context, id string) (clusterModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, plan)
		},
	}.run(ctx, create, req.Plan, &resp.State, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		keepDestroySettings(ctx, plan, &resp.State, &resp.Diagnostics)
	}
	if found {
		resp.Diagnostics.Append(recordUID(ctx, resp.Private, created.uid)...)
	}
	// Recorded whenever the cluster is in state, tainted included, since the
	// create carried the users either way.
	if !resp.State.Raw.IsNull() {
		resp.Diagnostics.Append(recordSSHAccess(ctx, resp.Private)...)
	}
}

func (r *clusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State, &req.State)
	var prior clusterModel
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
	if found && !refreshedOwnObject(ctx, req.Private, resp, "cluster", prior.ID.ValueString(), state.uid) {
		return
	}
	if found && prior.ClusterType.IsNull() {
		state.ClusterType = importedClusterTypeRef(ctx, r.meta, target.region, state.ClusterType)
	}
	// Here and not in read, which a create and an update share: they hold
	// what was sent, or nothing new, and the next refresh is what checks it.
	if found {
		read, reported, diags := r.readSSHAccess(ctx, target, state.ID.ValueString(), prior.SSHAccess)
		resp.Diagnostics.Append(diags...)
		// A delete releases the users with the node pools, and a failed
		// teardown leaves them gone: see teardown_read.go.
		released := slices.Contains(clusterStates.released, state.State.ValueString())
		var held bool
		state.SSHAccess, held = heldThroughTeardown(released, noSSHUsers(read), read, clusterSSHAccessState(prior.SSHAccess))
		if held && state.State.ValueString() == string(k0rdent.ClusterStateFailed) {
			resp.Diagnostics.AddWarning(
				"The platform reports no SSH users on this failed cluster",
				fmt.Sprintf("%q is `failed` and the platform answers no SSH users for it, which is what a "+
					"delete whose teardown failed leaves. State keeps the configured `ssh_access` so a "+
					"destroy can run again. If the cluster is meant to stay, only the platform's record "+
					"of those users is gone: SSH access is set when a node is provisioned and isn't "+
					"removed from a running one, so its nodes may still take their keys. To revoke them, "+
					"destroy the cluster or replace it with `terraform apply -replace`.", state.ID.ValueString()),
			)
		}
		resp.Diagnostics.Append(recordReleased(ctx, resp.Private, held)...)
		if reported {
			resp.Diagnostics.Append(recordSSHAccess(ctx, resp.Private)...)
		}
	}
	if !found {
		// Gone. Dropping it from state is the provider's call, not the
		// client's: the client hands back the 404 rather than collapsing it to
		// a nil result, which here is the difference between no change and
		// destroy and recreate.
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *clusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan, state clusterModel
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
	if (resumable[clusterModel]{
		kind: "cluster", resourceName: ResourceName(clusterResourceSuffix),
		timeout: plan.Timeouts.Create, fallback: clusterStates.timeout(r.meta.Wait),
		wait: func(ctx context.Context) error {
			return r.waitForSettled(ctx, target, id, "the cluster to finish creating")
		},
		read: func(ctx context.Context) (clusterModel, bool, diag.Diagnostics) {
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

	// A ceiling unknown at plan time got past neverLowered. Lower than the
	// cluster's, it's refused before anything is sent, since the platform
	// would refuse it and sending the rest would leave half an update.
	if from, to, lowered := storageLowered(plan, state); lowered {
		addLoweredCeilingError(&resp.Diagnostics, from, to)
		return
	}

	// Two different operations, chosen by what moved. `updateCluster` carries
	// the metadata fields; node counts go through `scaleCluster`, which is a
	// separate operation with its own validation.
	md, metadataChanged, diags := plan.metadata().patch(ctx, state.metadata())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Nothing the platform holds is changing: `on_destroy_busy` or a new
	// `timeouts` block on their own, which is how a configuration changes what
	// a later destroy does, or a value the platform reads as the same, such as
	// `cluster_type` pinned to the version already bound. The plan is recorded,
	// with prior state for what it left unknown, and nothing is sent or waited
	// for. Waiting here would sit out the
	// very operation that a switch from `wait` to `cancel` is made to stop
	// waiting for. `allow_replace` alone never reaches here
	// (replace_guard_resource.go).
	if !metadataChanged && onlyProviderSettingsChanged(plan, state) {
		next := planOverPrior(plan, state)
		resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
		return
	}

	leftFailed := false
	if metadataChanged {
		// A PATCH's tags replace the platform's. The create token isn't in
		// state, so it's read back and sent with them.
		if md.Tags != nil {
			got, err := r.meta.Client.GetClusterWithResponse(ctx, target.region, target.project, id, nil)
			if err != nil || got.JSON200 == nil {
				var respBody []byte
				var httpResp *http.Response
				if got != nil {
					respBody, httpResp = got.Body, got.HTTPResponse
				}
				addAPIError(&resp.Diagnostics, "read the cluster before updating its tags", httpResp, respBody, err)
				return
			}
			md.Tags = keepCreateToken(md.Tags, createTokenOf(presentMap(got.JSON200.Tags)))
		}
		body := k0rdent.ClusterUpdateRequest{
			DisplayName: md.DisplayName,
			Description: md.Description,
			Tags:        md.Tags,
		}

		updated, err := r.meta.Client.UpdateClusterWithResponse(ctx, target.region, target.project, id, body)
		// 200 for the synchronous fields, 202 when the platform queues a
		// workflow. Checking only JSON200 breaks on the async path with a nil
		// that looks like a malformed body.
		if err != nil || (updated.JSON200 == nil && updated.JSON202 == nil) {
			var respBody []byte
			var httpResp *http.Response
			if updated != nil {
				respBody, httpResp = updated.Body, updated.HTTPResponse
			}
			addAPIError(&resp.Diagnostics, "update the cluster", httpResp, respBody, err)
			return
		}
		// The platform leaves a `failed` cluster `failed` through this PATCH.
		// One that failed since the plan read it is still waited on, which
		// reports why.
		leftFailed = state.State.ValueString() == string(k0rdent.ClusterStateFailed) &&
			updated.JSON200 != nil && updated.JSON200.State != nil &&
			*updated.JSON200.State == k0rdent.ClusterStateFailed
	}

	// The security groups go second, as a PATCH of their own: the platform
	// answers 422 to `securityGroups` sent alongside any other field. The
	// change is asynchronous, so it is waited out before a scale or an
	// upgrade, which would otherwise meet a cluster that is `updating`.
	//
	// queued says a change went that the platform works on after answering.
	queued := false
	if securityGroupsChanged(plan, state) {
		queued = true
		resp.Diagnostics.Append(r.setSecurityGroups(ctx, target, id, plan.SecurityGroups)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// The storage ceiling goes after the groups, which are waited out, since
	// a raise is refused while the cluster isn't `active`. The platform
	// refuses `storage` alongside `securityGroups` in any case.
	if storageRaised(plan, state) {
		queued = true
		resp.Diagnostics.Append(r.raiseStorage(ctx, target, id, plan.StorageCeilingGiB)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// The plan refuses a refRefused change, so one never reaches here. Were it
	// to, the upgrade waits on the cluster and reports what it finds, which is
	// what this did before plan and apply shared the classification.
	switch change, _ := classifyRefChange(plan.ClusterType.ValueString(), boundRef(state),
		k0rdent.ClusterState(state.State.ValueString())); change {
	case refUpgrade, refRefused:
		// The upgrade makes the node count changes too, each before or after
		// the step whose bounds admit it.
		queued = true
		resp.Diagnostics.Append(r.upgrade(ctx, target, id, plan, &resp.State)...)
	default:
		want, have, d := r.poolCounts(ctx, target, id, plan, state)
		resp.Diagnostics.Append(d...)
		if !resp.Diagnostics.HasError() {
			up, down := poolChanges(want, have)
			queued = queued || len(up.NodePools)+len(down.NodePools) > 0
			resp.Diagnostics.Append(r.scaleEach(ctx, target, id, up, down)...)
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	// Nothing went to the platform, such as a node_pool that stops naming a
	// pool, which an import makes common. Waiting would sit out whatever the
	// cluster is doing, a create included, for a change that was never sent.
	if !queued && !metadataChanged {
		next := planOverPrior(plan, state)
		resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
		return
	}

	// Metadata alone on a `failed` cluster has nothing to wait for.
	if queued || !leftFailed {
		if err := r.waitForSettled(ctx, target, id, "the cluster to finish updating"); err != nil {
			addClusterWaitError(&resp.Diagnostics, "The cluster did not settle after the change", changeKeepsRunning, err)
			return
		}
	}

	// After an upgrade, state holds what its last pass read, the Kubernetes
	// version included, so the version isn't read twice.
	prior := recordedOver(ctx, plan, &resp.State)
	next, found, diags, err := settledRead(ctx, plan.State, clusterStates.settle,
		func(ctx context.Context) (clusterModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, prior)
		},
		func(m clusterModel) types.String { return m.State },
		func(ctx context.Context) error {
			return r.waitForSettled(ctx, target, id, "the cluster to finish updating")
		})
	resp.Diagnostics.Append(diags...)
	if err != nil {
		addClusterWaitError(&resp.Diagnostics, "The cluster did not settle after the change", changeKeepsRunning, err)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the provider's conclusion from two calls, as in createFinish.
		resp.Diagnostics.AddError(
			"The cluster disappeared during the update",
			fmt.Sprintf("Reading %q after the change answered 404.", id),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

// poolCounts returns the counts the plan wants and the ones the cluster has.
// A pool the plan names that prior state does not is compared with the count
// the platform reports, since growing and shrinking go as separate requests.
func (r *clusterResource) poolCounts(ctx context.Context, target clusterTarget, id string, plan, state clusterModel) (want, have map[string]int, diags diag.Diagnostics) {
	want, d := clusterPoolsFromModel(ctx, plan.NodePool)
	diags.Append(d...)
	have, d = clusterPoolsFromModel(ctx, state.NodePool)
	diags.Append(d...)
	if diags.HasError() {
		return nil, nil, diags
	}
	for pool := range want {
		if _, known := have[pool]; known {
			continue
		}
		settled, ok := r.settledCounts(ctx, target, id, &diags)
		if !ok {
			return nil, nil, diags
		}
		for p, n := range settled {
			if _, known := have[p]; !known {
				have[p] = n
			}
		}
		break
	}
	return want, have, diags
}

// poolChanges returns the node-count changes from have to want, as increases
// and decreases.
//
// The API constrains a scale in three ways that shape this: a target equal to
// the settled count is rejected, an unknown pool id is rejected, and a single
// request may not mix increases with decreases. So only changed pools are
// sent, and increases and decreases go as separate requests.
func poolChanges(want, have map[string]int) (up, down k0rdent.ClusterScaleRequest) {
	for _, poolID := range sortedKeys(want) {
		wanted := want[poolID]
		current, known := have[poolID]
		if known && current == wanted {
			continue
		}
		entry := zeroElem(up.NodePools)
		entry.Id = poolID
		entry.NodeCount = wanted
		if known && wanted < current {
			down.NodePools = append(down.NodePools, entry)
		} else {
			up.NodePools = append(up.NodePools, entry)
		}
	}
	return up, down
}

// scaleEach sends each non-empty request in turn, waiting each out before the
// next: a second request against a running workflow is answered 409.
func (r *clusterResource) scaleEach(ctx context.Context, target clusterTarget, id string, requests ...k0rdent.ClusterScaleRequest) diag.Diagnostics {
	var diags diag.Diagnostics
	for _, body := range requests {
		if len(body.NodePools) == 0 {
			continue
		}
		diags.Append(r.scaleOnce(ctx, target, id, body)...)
		if diags.HasError() {
			return diags
		}
	}
	return diags
}

func (r *clusterResource) scaleOnce(ctx context.Context, target clusterTarget, id string, body k0rdent.ClusterScaleRequest) diag.Diagnostics {
	var diags diag.Diagnostics

	// A refusal that scaleMayRetry admits is waited out.
	var scaled *k0rdent.ScaleClusterResponse
	err := awaitAdmitted(ctx, r.meta.Wait, "the cluster to admit the scale",
		func(ctx context.Context) (*http.Response, []byte, error) {
			var err error
			scaled, err = r.meta.Client.ScaleClusterWithResponse(ctx, target.region, target.project, id, body)
			if err != nil {
				return nil, nil, err
			}
			return scaled.HTTPResponse, scaled.Body, nil
		},
		func(ctx context.Context, resp *http.Response, raw []byte) (bool, *k0rdent.APIError, error) {
			if scaled.JSON200 != nil {
				return true, nil, nil
			}
			checked := k0rdent.CheckResponse(resp, raw)
			var refusal *k0rdent.APIError
			if errors.As(checked, &refusal) && scaleMayRetry(refusal, func() (k0rdent.ClusterState, bool) {
				got, readErr := r.meta.Client.GetClusterWithResponse(ctx, target.region, target.project, id, nil)
				if readErr != nil || got.JSON200 == nil || got.JSON200.State == nil {
					return "", false
				}
				return *got.JSON200.State, true
			}) {
				return false, refusal, nil
			}
			if checked == nil {
				checked = answeredWithout(resp, "cluster")
			}
			return false, nil, checked
		})
	if err != nil {
		addWaitError(&diags, "Could not scale the cluster", err)
		return diags
	}
	if err := r.waitForSettled(ctx, target, id, "the cluster to finish scaling"); err != nil {
		addClusterWaitError(&diags, "The cluster did not settle after scaling", changeKeepsRunning, err)
	}
	return diags
}

func (r *clusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, state.Timeouts.Delete, clusterStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	if !r.startDelete(ctx, target, id, destroyBusyOf(state.OnDestroyBusy), &resp.Diagnostics) {
		return
	}

	clusterStates.reportDeleteWait(&resp.Diagnostics, r.waitForDeleted(ctx, target, id))
}

// ImportState accepts either the cluster id on its own, using the provider's
// configured region and project, or the fully qualified `region/project/id`.
//
// An import id shape cannot be changed once users have scripted it, so both
// forms exist on purpose. The bare id is what someone types by hand against a
// provider they have already configured; the triple is what a script emits when
// it manages several regions. An identity names the same parts, on Terraform
// 1.12 and later.
func (r *clusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importScoped(ctx, r.meta, importForm{
		resource: ResourceName(clusterResourceSuffix),
		key:      "id",
	}, req, resp)
}

// IdentitySchema is what an import block's `identity` takes.
func (r *clusterResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = scopedIdentitySchema("id", "")
}

// read fetches the cluster through the single GET and maps it onto the model.
//
// prior is the model the new state is reconciled against: prior state on a
// refresh, the plan on a create or update. It is needed because several fields
// round-trip ambiguously and the only way to tell "the user asked for nothing"
// from "the platform dropped something" is to know what was asked for.
func (r *clusterResource) read(ctx context.Context, target clusterTarget, id string, prior clusterModel) (clusterModel, bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	got, err := r.meta.Client.GetClusterWithResponse(ctx, target.region, target.project, id, nil)
	if err != nil {
		addAPIError(&diags, "read the cluster", nil, nil, err)
		return clusterModel{}, false, diags
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		return clusterModel{}, false, diags
	}
	if got.JSON200 == nil {
		addAPIError(&diags, "read the cluster", got.HTTPResponse, got.Body, nil)
		return clusterModel{}, false, diags
	}

	out, d := clusterStateFrom(ctx, *got.JSON200, prior, r.meta.defaultTags())
	diags.Append(d...)
	// A ClusterType version is a snapshot that doesn't change, so a cluster
	// still bound to the one prior recorded keeps the Kubernetes version
	// recorded with it, and only a new binding asks again.
	if resolved := out.ClusterTypeResolved.ValueString(); resolved != "" &&
		prior.ClusterTypeResolved.ValueString() == resolved && !prior.KubernetesVersion.IsUnknown() && !prior.KubernetesVersion.IsNull() {
		out.KubernetesVersion = prior.KubernetesVersion
	} else {
		out.KubernetesVersion = r.meta.boundKubernetesVersion(ctx, target.region, resolved, &diags)
	}
	out.VPC = types.StringNull()
	if got.JSON200.Uid != nil {
		uid := got.JSON200.Uid.String()
		out.uid = uid
		out.VPC, d = ownedVPC(ctx, r.meta, target, k0rdent.ListVPCsParams{ClusterId: &uid})
		diags.Append(d...)
	}
	return out, true, diags
}

// clusterStateFrom maps a Cluster onto the Terraform model.
//
// Split out from read so it can be tested against a literal Cluster without a
// server, which is where the nil branches get their coverage: six of Cluster's
// nine required properties arrive as Go pointers because they are also
// `readOnly`, so a nil is a contract violation the generated client accepts
// silently. Every dereference below has to decide what a nil
// means, and none of them may panic in someone's apply.
func clusterStateFrom(ctx context.Context, c k0rdent.Cluster, prior clusterModel, defaults defaultTags) (clusterModel, diag.Diagnostics) {
	// The create token is the provider's, not the user's, so it never shows.
	tags, diags := tagAnswerOfRequired(c.Tags).forResource(prior.Tags)
	md := prior.metadata().read(metadataAnswer{DisplayName: c.DisplayName, Description: c.Description, Tags: tags, Defaults: defaults})

	out := clusterModel{
		OnDestroyBusy: prior.OnDestroyBusy,

		ID: types.StringValue(c.Id),
		// From prior state: the response does not echo them.
		Region:      prior.Region,
		Timeouts:    prior.Timeouts,
		SSHAccess:   clusterSSHAccessState(prior.SSHAccess),
		Project:     prior.Project,
		DisplayName: md.DisplayName,
		Description: md.Description,
		Tags:        md.Tags,
		TagsAll:     md.TagsAll,
		SelfLink:    stringOrNull(c.SelfLink),
		State:       types.StringNull(),
		NodeCount:   types.Int64Null(),
	}
	if c.State != nil {
		out.State = types.StringValue(string(*c.State))
	}

	// `clusterType` is where the round trip is genuinely undefined, so it is
	// the one field that is not simply copied. Keep the configured value when
	// the returned one is the same binding under either documented reading, and
	// always record what the platform actually said.
	returned := ""
	if c.ClusterType != nil {
		returned = *c.ClusterType
	}
	out.ClusterTypeResolved = types.StringValue(returned)
	configured := prior.ClusterType.ValueString()
	switch {
	case configured == "":
		// Import: there is nothing to preserve, so the platform's value is the
		// configuration. A plan straight after import will say whether it
		// matches the `.tf` the user wrote.
		out.ClusterType = types.StringValue(returned)
	case returned == "":
		// The contract marks `clusterType` required, and describes it as
		// "Omitted for pre-cutover clusters that predate the ClusterType
		// binding" in the same breath. Keep the configured value rather than
		// null a required argument out of state on a cluster that is otherwise
		// fine.
		out.ClusterType = prior.ClusterType
		diags.AddWarning(
			"The cluster did not report a cluster type",
			fmt.Sprintf("`getCluster` returned no `clusterType` for %q. The configured value is kept "+
				"in state. The contract marks the field required and also describes it as omitted "+
				"for clusters that predate the ClusterType binding, so this may be either.", c.Id),
		)
	case clusterTypeRefEquivalent(configured, returned) != refMismatch:
		out.ClusterType = prior.ClusterType
	default:
		// A real difference. Write what the platform says and let Terraform
		// propose the replacement, because `cluster_type` is immutable and the
		// cluster is genuinely bound to something else.
		out.ClusterType = types.StringValue(returned)
	}

	// Node pools. The platform returns every materialized pool; the model holds
	// the sparse set this configuration manages. Narrow the read to the keys
	// already known, so a pool nobody asked about does not appear in state and
	// then look like something to remove. With nothing known, as on an import,
	// take them all, which is the only way the user learns what exists.
	settled := map[string]int64{}
	total := int64(0)
	for _, p := range c.NodePools {
		settled[p.Id] = int64(p.NodeCount)
		total += int64(p.NodeCount)
	}
	if c.NodeCount != nil {
		out.NodeCount = types.Int64Value(int64(*c.NodeCount))
	} else {
		// Optional and read-only; derive it rather than leave a null where a
		// number obviously belongs.
		out.NodeCount = types.Int64Value(total)
	}

	keep := settled
	released := clusterStates.releasedInputs(c) && len(c.NodePools) == 0
	if !released && !prior.NodePool.IsNull() && !prior.NodePool.IsUnknown() {
		known, d := clusterPoolsFromModel(ctx, prior.NodePool)
		diags.Append(d...)
		keep = map[string]int64{}
		for id := range known {
			if count, ok := settled[id]; ok {
				keep[id] = count
			} else {
				// The configuration names a pool the cluster does not have.
				// Keep it visible so the plan proposes it rather than silently
				// dropping it: the API answers 422 for an unknown pool id, and
				// that error is more use than a vanishing attribute.
				keep[id] = int64(known[id])
				diags.AddWarning(
					"A configured node pool is not on the cluster",
					fmt.Sprintf("`node_pool[%q]` is in the configuration for %q but the platform "+
						"reports no pool with that id. Pool ids come from the bound ClusterType "+
						"version.", id, c.Id),
				)
			}
		}
	}

	pools := map[string]clusterNodePoolModel{}
	for id, count := range keep {
		pools[id] = clusterNodePoolModel{NodeCount: types.Int64Value(count)}
	}
	m, d := types.MapValueFrom(ctx, clusterNodePoolObjectType(), pools)
	diags.Append(d...)
	// A delete took the pools, not the configuration: see teardown_read.go.
	var held bool
	out.NodePool, held = heldThroughTeardown(released, true, m, prior.NodePool)
	if held && c.State != nil && *c.State == k0rdent.ClusterStateFailed {
		diags.AddWarning(
			"The platform reports no node pools on this failed cluster",
			fmt.Sprintf("%q is `failed` and the platform answers no node pools for it, which is what a "+
				"delete whose teardown failed leaves. State keeps the configured `node_pool` so a "+
				"destroy can run again. If the cluster is meant to stay, it no longer has those "+
				"pools: destroy it, or replace it with `terraform apply -replace`.", c.Id),
		)
	}

	// Security groups, always as the platform reports them and in its order.
	// The field is required and `[]` when none, and an omission is read the
	// same way, so the attribute is never null after a read and a configured
	// `[]` matches a cluster with no groups.
	groups := c.SecurityGroups
	if groups == nil {
		groups = []string{}
	}
	sg, d := types.ListValueFrom(ctx, types.StringType, groups)
	diags.Append(d...)
	out.SecurityGroups = sg
	out.StorageCeilingGiB = storageCeilingFrom(c)

	return out, diags
}

// waitForSettled blocks until the cluster leaves its transient states. A wait
// that ends with the cluster read `failed` says why as far as the platform's
// runtime event log does: see explainFailure. That covers every wait on a
// cluster, a create's, a scale's and each upgrade step's alike.
func (r *clusterResource) waitForSettled(ctx context.Context, target clusterTarget, id, what string) error {
	read := r.reader(target, id)
	events := &eventFollower{r: r, target: target, id: id}
	var failed bool
	var execution string
	err := clusterStates.awaitSettled(ctx, r.meta.Wait, what, func(ctx context.Context) (*k0rdent.Cluster, *http.Response, []byte, error) {
		body, resp, raw, err := read(ctx)
		if body != nil {
			failed = body.State != nil && *body.State == k0rdent.ClusterStateFailed
			execution = executionOf(body)
		}
		events.poll(ctx)
		return body, resp, raw, err
	})
	return r.explainWait(ctx, target, id, err, failed, execution, events)
}

// waitForDeleted blocks until the cluster is gone, following the runtime
// event log and explaining a failure or a timeout the way waitForSettled does.
func (r *clusterResource) waitForDeleted(ctx context.Context, target clusterTarget, id string) error {
	read := r.reader(target, id)
	events := &eventFollower{r: r, target: target, id: id}
	var failed bool
	var execution string
	err := clusterStates.awaitDeleted(ctx, r.meta.Wait, "the cluster to finish deleting", func(ctx context.Context) (*k0rdent.Cluster, *http.Response, []byte, error) {
		body, resp, raw, err := read(ctx)
		if body != nil {
			failed = body.State != nil && *body.State == k0rdent.ClusterStateFailed
			execution = executionOf(body)
		}
		events.poll(ctx)
		return body, resp, raw, err
	})
	return r.explainWait(ctx, target, id, err, failed, execution, events)
}

// explainWait adds what the platform can say about a wait that didn't end
// well: the event log's warnings and errors for a `failed` cluster, its last
// line for a timeout, and the operation's execution id either way.
func (r *clusterResource) explainWait(ctx context.Context, target clusterTarget, id string, err error, failed bool, execution string, events *eventFollower) error {
	switch {
	case err == nil:
		return nil
	case failed:
		err = r.explainFailure(ctx, target, id, err)
	case errors.Is(err, errWaitTimeout):
		err = events.explainTimeout(err)
	default:
		return err
	}
	return namesExecution(err, execution)
}

// executionOf is the cluster's `operation.executionId`, or "".
func executionOf(c *k0rdent.Cluster) string {
	if c.Operation == nil || c.Operation.ExecutionId == nil {
		return ""
	}
	return *c.Operation.ExecutionId
}

// reader reads the cluster for a wait.
func (r *clusterResource) reader(target clusterTarget, id string) stateRead[k0rdent.Cluster] {
	return func(ctx context.Context) (*k0rdent.Cluster, *http.Response, []byte, error) {
		got, err := r.meta.Client.GetClusterWithResponse(ctx, target.region, target.project, id, nil)
		if err != nil {
			return nil, nil, nil, err
		}
		return got.JSON200, got.HTTPResponse, got.Body, nil
	}
}

// withTimeout applies the resource's own timeout for one operation, falling
// back to the provider's operation_timeout. A per-resource default is the
// difference between one slow cluster and every cluster in the configuration
// inheriting its wait.
func withTimeout(ctx context.Context, get func(context.Context, time.Duration) (time.Duration, diag.Diagnostics), fallback time.Duration, diags *diag.Diagnostics) (context.Context, context.CancelFunc) {
	d, ds := get(ctx, fallback)
	diags.Append(ds...)
	if d <= 0 {
		d = fallback
	}
	return context.WithTimeout(ctx, d)
}

// securityGroupsChanged reports whether the plan asks for a different binding.
// An unknown plan value is the attribute left out with nothing in state yet,
// which asks for nothing.
func securityGroupsChanged(plan, state clusterModel) bool {
	if plan.SecurityGroups.IsNull() || plan.SecurityGroups.IsUnknown() {
		return false
	}
	return !plan.SecurityGroups.Equal(state.SecurityGroups)
}

// setSecurityGroups replaces the cluster's whole binding and waits for the
// cluster to settle. The platform answers 202 and re-renders every network
// bundle the cluster has, which moves it to `updating` until that finishes.
func (r *clusterResource) setSecurityGroups(ctx context.Context, target clusterTarget, id string, list types.List) diag.Diagnostics {
	var diags diag.Diagnostics
	groups, d := optionalStringList(ctx, list)
	diags.Append(d...)
	if diags.HasError() || groups == nil {
		return diags
	}

	updated, err := r.meta.Client.UpdateClusterWithResponse(ctx, target.region, target.project, id,
		k0rdent.ClusterUpdateRequest{SecurityGroups: groups})
	if err != nil || (updated.JSON200 == nil && updated.JSON202 == nil) {
		var respBody []byte
		var httpResp *http.Response
		if updated != nil {
			respBody, httpResp = updated.Body, updated.HTTPResponse
		}
		addAPIError(&diags, "change the cluster's security groups", httpResp, respBody, err)
		return diags
	}
	if err := r.waitForSettled(ctx, target, id, "the cluster to apply its security groups"); err != nil {
		addClusterWaitError(&diags, "The cluster did not settle after its security groups changed",
			changeKeepsRunning, err)
	}
	return diags
}

// clusterPoolsFromModel flattens the `node_pool` map to id -> count.
func clusterPoolsFromModel(ctx context.Context, m types.Map) (map[string]int, diag.Diagnostics) {
	var diags diag.Diagnostics
	out := map[string]int{}
	if m.IsNull() || m.IsUnknown() {
		return out, diags
	}
	var pools map[string]clusterNodePoolModel
	diags.Append(m.ElementsAs(ctx, &pools, false)...)
	for id, p := range pools {
		if p.NodeCount.IsNull() || p.NodeCount.IsUnknown() {
			continue
		}
		out[id] = int(p.NodeCount.ValueInt64())
	}
	return out, diags
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stateDetail(message *string, reason string) string {
	var parts []string
	if reason != "" {
		parts = append(parts, "reason "+reason)
	}
	if message != nil && *message != "" {
		parts = append(parts, *message)
	}
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, "; ")
}

func reasonString(r *k0rdent.StateReason) string {
	if r == nil {
		return ""
	}
	return string(*r)
}

// zeroElem returns the zero value of a slice's element type.
//
// It exists so that request bodies whose generated shape is an anonymous struct
// (`ClusterScaleRequest.NodePools` and the two `InstanceGroup` equivalents) can
// be built without this repository restating that shape. A literal spelling out
// the anonymous type's fields and JSON tags would compile until the day the
// field names matched but the tags did not.
func zeroElem[S ~[]E, E any](_ S) (zero E) { return }

// derefOrNil is the slice behind an optional generated list, nil when absent.
// With zeroElem it builds a list whose element type is an unnamed struct.
func derefOrNil[S ~[]E, E any](p *S) S {
	if p == nil {
		return nil
	}
	return *p
}
