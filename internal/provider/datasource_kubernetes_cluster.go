package provider

import (
	"context"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*clusterDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*clusterDataSource)(nil)
)

const clusterDataSourceSuffix = "kubernetes_cluster"

func newClusterDataSource() datasource.DataSource { return &clusterDataSource{} }

// clusterDataSource reads a cluster this configuration does not manage.
//
// # Why this is not just the resource with Read
//
// It carries what the resource must not: health, the state message and reason,
// the hosted control plane's judgement and the current operation with its
// start and end times. Each of those moves without the configuration moving,
// so on the resource it would be a diff on every refresh. A data source is
// re-read every run and compared against nothing.
//
// # Why it exists at all
//
// A configuration in one state boundary has to be able to name a cluster
// managed in another without taking ownership of it. Passing the cluster's id
// to the kubeconfig data source as a bare string couples the two
// configurations through a literal and says nothing about whether the cluster
// is even up.
//
// # What it deliberately does not carry
//
// The control plane's CA certificate. The platform returns it beside the API
// server URL and a data source writes everything it reads into plain-text
// state, so it is left out under the same rule that keeps the kubeconfig body
// in an ephemeral resource. The URL alone is what a configuration needs to
// decide where to point something; the material to authenticate with belongs
// on the ephemeral path.
type clusterDataSource struct{ meta *Meta }

type clusterDataSourceModel struct {
	ID      types.String `tfsdk:"id"`
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`

	ClusterType     types.String `tfsdk:"cluster_type"`
	ClusterTypeName types.String `tfsdk:"cluster_type_name"`
	K8sVersion      types.String `tfsdk:"kubernetes_version"`
	DisplayName     types.String `tfsdk:"display_name"`
	Description     types.String `tfsdk:"description"`
	Tags            types.Map    `tfsdk:"tags"`
	SelfLink        types.String `tfsdk:"self_link"`

	State        types.String `tfsdk:"state"`
	StateMessage types.String `tfsdk:"state_message"`
	StateReason  types.String `tfsdk:"state_reason"`
	HealthState  types.String `tfsdk:"health_state"`
	HealthReason types.String `tfsdk:"health_reason"`

	APIEndpoint types.String `tfsdk:"api_endpoint"`

	ControlPlaneState      types.String `tfsdk:"control_plane_state"`
	ControlPlaneMessage    types.String `tfsdk:"control_plane_message"`
	ControlPlaneVersion    types.String `tfsdk:"control_plane_version"`
	ControlPlaneDesired    types.Int64  `tfsdk:"control_plane_desired_replicas"`
	ControlPlaneReadyCount types.Int64  `tfsdk:"control_plane_ready_replicas"`

	NextAvailableClusterTypeVersion types.String           `tfsdk:"next_available_cluster_type_version"`
	UpgradeTargetVersionID          types.String           `tfsdk:"upgrade_target_version_id"`
	Operation                       *clusterOperationModel `tfsdk:"operation"`

	NodeCount types.Int64                           `tfsdk:"node_count"`
	NodePools map[string]clusterDataSourcePoolModel `tfsdk:"node_pools"`
	SecGroups []types.String                        `tfsdk:"security_groups"`
}

type clusterOperationModel struct {
	ExecutionID     types.String `tfsdk:"execution_id"`
	Status          types.String `tfsdk:"status"`
	Reason          types.String `tfsdk:"reason"`
	Message         types.String `tfsdk:"message"`
	StartedAt       types.String `tfsdk:"started_at"`
	CompletedAt     types.String `tfsdk:"completed_at"`
	PercentComplete types.Int64  `tfsdk:"percent_complete"`
	CurrentStage    types.String `tfsdk:"current_stage"`
}

type clusterDataSourcePoolModel struct {
	DisplayName     types.String `tfsdk:"display_name"`
	NodeCount       types.Int64  `tfsdk:"node_count"`
	TargetNodeCount types.Int64  `tfsdk:"target_node_count"`
}

func (d *clusterDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(clusterDataSourceSuffix)
}

func (d *clusterDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One Kubernetes cluster, read rather than managed.\n\n" +
			"Use it to reference a cluster another configuration owns, without taking ownership of " +
			"it. It carries more than the `" + ResourceName(clusterResourceSuffix) + "` resource " +
			"does: health, the control plane's state, the platform's state message and the " +
			"current operation all change without the configuration changing, so the resource " +
			"cannot carry them without reporting drift on every refresh, and a read-only form " +
			"can.\n\n" +
			"An upgrade in progress shows as `upgrade_target_version_id`, with its phase and " +
			"start time under `operation`.\n\n" +
			"It does not return the control plane's CA certificate yet. The " +
			"`" + ResourceName("cluster_kubeconfig") + "` ephemeral resource carries it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The cluster's name, which is its id.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"cluster_type": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The ClusterType version this cluster is bound to, as the platform " +
					"reports it. Normally the pinned `/versions/{id}` form even where an unversioned " +
					"reference was supplied at creation.",
			},
			"kubernetes_version": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The Kubernetes version the bound ClusterType version carries, " +
					"such as `v1.35.1+k0s.0`, read from that version's `k8s.k0sVersion`. It's the " +
					"version the cluster is bound to: `control_plane_version` is what a hosted control " +
					"plane runs, and lags it while an upgrade rolls out. Null when the bound version " +
					"carries no Kubernetes configuration, or the platform no longer finds it.",
			},
			"cluster_type_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Display name of the bound ClusterType version, as of binding.",
			},
			"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
			"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-form text describing it, for people to read."},
			"tags": schema.MapAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "User-defined labels, as key-value pairs.",
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this cluster.",
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle state. `active` is settled and usable.",
			},
			"state_message": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The platform's own explanation of the current state. Null when " +
					"there is nothing to explain.",
			},
			"state_reason": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Machine-readable cause of a `failed` state, such as " +
					"`INSUFFICIENT_CAPACITY`. Stable enough to branch on, unlike `state_message`. Null " +
					"when no failure is recorded.",
			},
			"health_state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Observed health, which is reported separately from `state` and " +
					"never drives it. `unknown` until the platform first reports it.",
			},
			"health_reason": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Why the health is what it is. Null when healthy.",
			},
			"api_endpoint": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "URL of the Kubernetes API server. Null until the control plane " +
					"has one. The certificate to trust it with is not returned here; see the " +
					"`" + ResourceName("cluster_kubeconfig") + "` ephemeral resource.",
			},
			"control_plane_state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The platform's judgement of a hosted control plane: `ready`, " +
					"`provisioning`, `progressing`, `degraded`, `unavailable`, `deleting` or `unknown`. " +
					"`ready` means the tenant API server answers. `progressing` covers both a scale and " +
					"a version rollout; compare `control_plane_version` with the bound ClusterType " +
					"version to tell them apart. Null until a hosted control plane has been observed, " +
					"and always null on a standalone one.",
			},
			"control_plane_message": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Why `control_plane_state` is what it is. Free text to show, not to " +
					"parse. Null whenever `control_plane_state` is.",
			},
			"control_plane_version": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The version the hosted control plane is actually running. It lags " +
					"the bound ClusterType version while an upgrade rolls out. Empty until a replica " +
					"is ready, and null whenever `control_plane_state` is.",
			},
			"control_plane_desired_replicas": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Control plane replicas the hosted control plane's spec asks for. " +
					"Null whenever `control_plane_state` is.",
			},
			"control_plane_ready_replicas": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Control plane replicas passing their readiness probe, to read " +
					"against `control_plane_desired_replicas`. Null whenever `control_plane_state` is, " +
					"and a 0 here is a real reading.",
			},
			"next_available_cluster_type_version": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The ClusterType version this cluster could move to, when one is " +
					"available. Null when it is on the latest.",
			},
			"upgrade_target_version_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Id of the ClusterType version an upgrade in flight is moving to. " +
					"Null when no upgrade is in flight. On success `cluster_type` moves to it; on " +
					"failure this is cleared and `cluster_type` stays where it was.",
			},
			"operation": schema.SingleNestedAttribute{
				Computed: true,
				MarkdownDescription: "The cluster's current or last lifecycle operation: a create, a " +
					"scale, an upgrade or a delete. This is where an upgrade's phase and start time " +
					"are read. Null on a cluster that has never run one.",
				Attributes: map[string]schema.Attribute{
					"execution_id": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "Identifier of the operation, stable for its lifetime. Quote it when asking the platform team what happened.",
					},
					"status": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "`running`, `succeeded`, `failed` or `cancelled`. Not final: " +
							"a cluster the platform force-failed can still converge and move this back to " +
							"`succeeded`.",
					},
					"reason": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "Machine-readable cause, such as `TIMEOUT`. Stable enough to branch on. Null when none is recorded.",
					},
					"message": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "The platform's own detail about the operation. Read it, do not branch on it.",
					},
					"started_at": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "When the operation began, in RFC 3339. Subtract it from " +
							"`plantimestamp()` for how long it has been running. Null on an operation " +
							"older than the field.",
					},
					"completed_at": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "When the operation ended, in RFC 3339. Null while it runs, " +
							"and also on an operation older than the field, so read `status` to know " +
							"whether it is still going.",
					},
					"percent_complete": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "Share of the operation's stages done or skipped, rounded down. Null when the platform reports no stages.",
					},
					"current_stage": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "Key of the stage in progress, else the stage that failed, " +
							"else the next one pending, such as `reserving-hardware`. Null when none " +
							"applies.",
					},
				},
			},
			"node_count": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Total nodes across every pool the cluster has.",
			},
			"node_pools": schema.MapNestedAttribute{
				Computed: true,
				MarkdownDescription: "Every pool the platform created, including the ones a " +
					"configuration did not name, keyed by pool slug: the same key a cluster's " +
					"`node_pool` map uses.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"display_name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Display name, resolved from the ClusterType's pool.",
						},
						"node_count": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Settled number of nodes in the pool.",
						},
						"target_node_count": schema.Int64Attribute{
							Computed: true,
							MarkdownDescription: "Desired count while a scale is in flight. Null when " +
								"the pool is idle, in which case `node_count` is the whole story.",
						},
					},
				},
			},
			"security_groups": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Security groups applied to the cluster's instances. Order is " +
					"significant: it is the order the fabric applies them in.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *clusterDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *clusterDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data clusterDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	includeStages := true
	got, err := d.meta.Client.GetClusterWithResponse(ctx, target.region, target.project, data.ID.ValueString(),
		// Without it the operation carries no progress.
		&k0rdent.GetClusterParams{IncludeStages: &includeStages})
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the cluster", httpResp, body, err)
		return
	}
	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	resp.Diagnostics.Append(data.fill(ctx, *got.JSON200)...)
	data.K8sVersion = d.meta.boundKubernetesVersion(ctx, target.region, data.ClusterType.ValueString(), &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (data *clusterDataSourceModel) fill(ctx context.Context, c k0rdent.Cluster) diag.Diagnostics {
	var diags diag.Diagnostics
	data.ClusterType = stringOrNull(c.ClusterType)
	data.ClusterTypeName = stringOrNull(c.ClusterTypeName)
	data.DisplayName = stringOrNull(c.DisplayName)
	data.Description = stringOrNull(c.Description)
	data.SelfLink = stringOrNull(c.SelfLink)
	data.StateMessage = stringOrNull(c.StateMessage)
	data.NextAvailableClusterTypeVersion = stringOrNull(c.NextAvailableClusterTypeVersion)
	data.NodeCount = int64OrNull(c.NodeCount)

	data.State = enumOrNull(c.State)
	data.StateReason = enumOrNull(c.StateReason)
	data.UpgradeTargetVersionID = types.StringNull()
	if c.TargetClusterTypeVersionId != nil {
		data.UpgradeTargetVersionID = types.StringValue(c.TargetClusterTypeVersionId.String())
	}
	data.Operation = operationFrom(c.Operation)

	data.HealthState, data.HealthReason = types.StringNull(), types.StringNull()
	if c.Health != nil {
		if c.Health.State != nil {
			data.HealthState = types.StringValue(string(*c.Health.State))
		}
		data.HealthReason = stringOrNull(c.Health.Reason)
	}

	data.APIEndpoint = types.StringNull()
	data.ControlPlaneState, data.ControlPlaneMessage = types.StringNull(), types.StringNull()
	data.ControlPlaneVersion = types.StringNull()
	data.ControlPlaneDesired, data.ControlPlaneReadyCount = types.Int64Null(), types.Int64Null()
	if c.ControlPlane != nil {
		if c.ControlPlane.Endpoint != nil {
			data.APIEndpoint = stringOrNull(c.ControlPlane.Endpoint.Url)
		}
		if h := c.ControlPlane.Hosted; h != nil {
			data.ControlPlaneState = enumOrNull(h.State)
			data.ControlPlaneMessage = stringOrNull(h.Message)
			data.ControlPlaneVersion = stringOrNull(h.Version)
			data.ControlPlaneDesired = int64OrNull(h.DesiredReplicas)
			data.ControlPlaneReadyCount = int64OrNull(h.ReadyReplicas)
		}
	}

	data.Tags = tagAnswerOfRequired(c.Tags).forDataSource()

	data.NodePools = make(map[string]clusterDataSourcePoolModel, len(c.NodePools))
	for _, p := range c.NodePools {
		data.NodePools[p.Id] = clusterDataSourcePoolModel{
			DisplayName:     stringOrNull(p.DisplayName),
			NodeCount:       types.Int64Value(int64(p.NodeCount)),
			TargetNodeCount: int64OrNull(p.TargetNodeCount),
		}
	}

	data.SecGroups = make([]types.String, 0, len(c.SecurityGroups))
	for _, g := range c.SecurityGroups {
		data.SecGroups = append(data.SecGroups, types.StringValue(g))
	}

	return diags
}

func operationFrom(op *k0rdent.Operation) *clusterOperationModel {
	if op == nil {
		return nil
	}
	out := &clusterOperationModel{
		ExecutionID:     stringOrNull(op.ExecutionId),
		Status:          enumOrNull(op.Status),
		Reason:          enumOrNull(op.Reason),
		Message:         stringOrNull(op.Message),
		StartedAt:       timeOrNull(op.StartedAt),
		CompletedAt:     timeOrNull(op.CompletedAt),
		PercentComplete: types.Int64Null(),
		CurrentStage:    types.StringNull(),
	}
	if op.Progress != nil {
		out.PercentComplete = int64OrNull(op.Progress.PercentComplete)
		out.CurrentStage = stringOrNull(op.Progress.Current)
	}
	return out
}

// enumOrNull flattens one of the contract's named string enums.
func enumOrNull[T ~string](p *T) types.String {
	if p == nil {
		return types.StringNull()
	}
	return types.StringValue(string(*p))
}

// timeOrNull renders a timestamp in UTC, so the same instant always reads the
// same.
func timeOrNull(p *time.Time) types.String {
	if p == nil {
		return types.StringNull()
	}
	return types.StringValue(p.UTC().Format(time.RFC3339))
}
