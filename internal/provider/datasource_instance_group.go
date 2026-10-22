package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*instanceGroupDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*instanceGroupDataSource)(nil)
)

const instanceGroupDataSourceSuffix = "instance_group"

func newInstanceGroupDataSource() datasource.DataSource { return &instanceGroupDataSource{} }

// instanceGroupDataSource reads a group of bare hosts this configuration does
// not manage. See the cluster data source for why a read-only form carries
// more than its resource does.
//
// It carries the group's SSH access list, the same shape the resource takes.
// Those are the public keys and usernames the group was created with, never a
// private key, and the resource already holds them in state. Here they let a
// configuration that reads a group another one owns see who can log in.
type instanceGroupDataSource struct{ meta *Meta }

type instanceGroupDataSourceModel struct {
	ID      types.String `tfsdk:"id"`
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`

	ClusterType types.String `tfsdk:"cluster_type"`
	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`
	SelfLink    types.String `tfsdk:"self_link"`

	State        types.String `tfsdk:"state"`
	StateMessage types.String `tfsdk:"state_message"`
	StateReason  types.String `tfsdk:"state_reason"`
	ExecutionID  types.String `tfsdk:"execution_id"`

	ProgressUpdatedAt types.String `tfsdk:"progress_updated_at"`

	SSHAccess types.Set `tfsdk:"ssh_access"`

	NodePools map[string]instanceGroupDataSourcePoolModel `tfsdk:"node_pools"`
	SecGroups []types.String                              `tfsdk:"security_groups"`
}

type instanceGroupDataSourcePoolModel struct {
	MachineType      types.String `tfsdk:"machine_type"`
	DesiredCount     types.Int64  `tfsdk:"desired_count"`
	ReadyCount       types.Int64  `tfsdk:"ready_count"`
	NotReadyCount    types.Int64  `tfsdk:"not_ready_count"`
	ReservationPhase types.String `tfsdk:"reservation_phase"`
}

func (d *instanceGroupDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(instanceGroupDataSourceSuffix)
}

func (d *instanceGroupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One instance group, read rather than managed.\n\n" +
			"Use it to reference a group another configuration owns. It carries the per-pool ready " +
			"counts and the reservation phase, which is how far provisioning has actually got: the " +
			"`" + ResourceName(instanceGroupResourceSuffix) + "` resource cannot carry those " +
			"without reporting drift every time hardware moves underneath it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Instance group name.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"cluster_type": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The bound ClusterType version, whose pools describe the hardware " +
					"this group is provisioned from.",
			},
			"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
			"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-form text describing it, for people to read."},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this instance group.",
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle state. `active` is settled.",
			},
			"state_message": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The platform's own explanation of the current state.",
			},
			"state_reason": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Machine-readable cause of the last recorded failure, such as " +
					"`INSUFFICIENT_CAPACITY`. Stable enough to branch on. Also set on a group that " +
					"settled back to `active` after a failed scale. Null when no failure is recorded.",
			},
			"execution_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The workflow execution behind an operation in progress, or behind " +
					"a recent failure. This is the handle to quote when asking the platform team what " +
					"happened. Null on a settled group that has nothing outstanding.",
			},
			"progress_updated_at": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "When the per-pool progress in `node_pools` last moved, in RFC " +
					"3339. A value that stops advancing while the group is in flight is a stalled " +
					"reservation. Null until the platform has recorded any progress.",
			},
			"node_pools": schema.MapNestedAttribute{
				Computed: true,
				MarkdownDescription: "Per-pool status as the platform reports it, keyed by the ClusterType " +
					"pool name. `ready_count + not_ready_count` is the hosts a pool actually holds, so " +
					"`desired_count` minus that is a shortfall the platform could not fill.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"machine_type": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "The machine type behind the pool." +
								knownGap("A name only. A tenant can't read what hardware or image it is."),
						},
						"desired_count": schema.Int64Attribute{
							Computed: true,
							MarkdownDescription: "Hosts asked for. Only a create or a scale changes it, so " +
								"it stays at the request when the platform found fewer hosts.",
						},
						"ready_count": schema.Int64Attribute{
							Computed: true,
							MarkdownDescription: "Hosts provisioned and usable now. Null when the platform " +
								"could not read the pool's reservation, never 0 in its place.",
						},
						"not_ready_count": schema.Int64Attribute{
							Computed: true,
							MarkdownDescription: "Hosts attached but not usable yet, or on their way out. " +
								"Null whenever `ready_count` is.",
						},
						"reservation_phase": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "How far the hardware reservation behind this pool has got, while the group is in flight.",
						},
					},
				},
			},
			"ssh_access": schema.SetNestedAttribute{
				Computed: true,
				MarkdownDescription: "Linux users written onto every host in the group when it was created, " +
					"each with the public keys allowed to log in as that user. Null when the group was " +
					"created without any, or when the platform does not return them.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"username":        schema.StringAttribute{Computed: true, MarkdownDescription: sshUsernameDescription},
						"ssh_public_keys": schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: sshPublicKeysDescription},
						"sudo":            schema.StringAttribute{Computed: true, MarkdownDescription: sshSudoDescription},
						"display_name":    schema.StringAttribute{Computed: true, MarkdownDescription: sshDisplayNameDescription},
					},
				},
			},
			"security_groups": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Security groups applied to the group's instances. Order is " +
					"significant: it is the order the fabric applies them in.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *instanceGroupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *instanceGroupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data instanceGroupDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	got, err := d.meta.Client.GetInstanceGroupWithResponse(ctx, target.region, target.project, data.ID.ValueString(), nil)
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the instance group", httpResp, body, err)
		return
	}
	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	resp.Diagnostics.Append(data.fill(ctx, *got.JSON200)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (data *instanceGroupDataSourceModel) fill(ctx context.Context, g k0rdent.InstanceGroup) diag.Diagnostics {
	data.ClusterType = types.StringValue(g.ClusterType)
	data.DisplayName = stringOrNull(g.DisplayName)
	data.Description = stringOrNull(g.Description)
	data.SelfLink = stringOrNull(g.SelfLink)
	data.StateMessage = stringOrNull(g.StateMessage)
	data.ExecutionID = stringOrNull(g.ExecutionId)

	data.State = enumOrNull(g.State)
	data.StateReason = enumOrNull(g.StateReason)
	data.ProgressUpdatedAt = timeOrNull(g.ProgressUpdatedAt)

	data.NodePools = map[string]instanceGroupDataSourcePoolModel{}
	if g.NodePools != nil {
		for _, p := range *g.NodePools {
			entry := instanceGroupDataSourcePoolModel{
				MachineType:      stringOrNull(p.MachineType),
				DesiredCount:     types.Int64Value(int64(p.DesiredCount)),
				ReadyCount:       int64OrNull(p.ReadyCount),
				NotReadyCount:    int64OrNull(p.NotReadyCount),
				ReservationPhase: types.StringNull(),
			}
			if p.ReservationPhase != nil {
				entry.ReservationPhase = types.StringValue(string(*p.ReservationPhase))
			}
			data.NodePools[p.NodePool] = entry
		}
	}

	data.SecGroups = make([]types.String, 0, len(g.SecurityGroups))
	for _, s := range g.SecurityGroups {
		data.SecGroups = append(data.SecGroups, types.StringValue(s))
	}

	var diags diag.Diagnostics
	data.SSHAccess, diags = sshAccessValue(ctx, g.SshAccess)
	return diags
}
