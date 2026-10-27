package provider

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure the implementation satisfies the framework interface at compile time.
var (
	_ provider.Provider                       = (*aiProvider)(nil)
	_ provider.ProviderWithEphemeralResources = (*aiProvider)(nil)
	_ provider.ProviderWithListResources      = (*aiProvider)(nil)
	_ provider.ProviderWithActions            = (*aiProvider)(nil)
)

// aiProvider is the provider implementation. It is unexported because the
// framework only ever sees it through New, and it is named for the product
// rather than the Terraform type name so that a rename does not reach it.
type aiProvider struct {
	// version is stamped at build time and reported to Terraform. It appears
	// in the user agent the client sends.
	version string

	// lookupEnv reads the environment. Tests replace it; nothing else does,
	// which is why it is a field rather than a constructor parameter — a
	// second public constructor would be a second way to build a provider.
	lookupEnv func(string) string

	// defaults is default_tags as Configure resolved the block, for the plan
	// modifiers of the resources this provider registers. See
	// providerDefaults.
	defaults     *providerDefaults
	defaultsOnce sync.Once
}

// sharedDefaults is the provider's providerDefaults, made on first use so a
// provider built in a test without New has one too.
func (p *aiProvider) sharedDefaults() *providerDefaults {
	p.defaultsOnce.Do(func() { p.defaults = &providerDefaults{} })
	return p.defaults
}

// getenv returns the environment reader, defaulting to the real one.
func (p *aiProvider) getenv() func(string) string {
	if p.lookupEnv != nil {
		return p.lookupEnv
	}
	return os.Getenv
}

// New returns the constructor Terraform's plugin server expects.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &aiProvider{version: version}
	}
}

// Metadata reports the provider type name. This is the one call that decides
// what users write in `required_providers` and what every resource name starts
// with, and it reads the name from a single constant on purpose — see names.go.
func (p *aiProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = ProviderTypeName
	resp.Version = p.version
}

// Schema declares the provider-level configuration block.
//
// # Three attributes, and one conspicuous absence
//
// Scoping in this API is not symmetrical, so the schema is not either. Region
// is a URL path segment. Project is a URL path segment on the subset of paths
// that are project-scoped, and it takes the project's slug. Organization is
// neither: the caller never sends it. The gateway resolves the owning
// organization from the membership tree in the validated token and injects it
// downstream, stripping any `x-k0r-*` header a client tries to set. An
// `organization` attribute would therefore accept a value, change nothing, and
// leave the user to discover that from a result they did not expect. It is
// omitted on purpose, and a test asserts it stays omitted.
//
// # No credential attribute
//
// The requirement is "no credential in a file", and the schema is where
// that is either honoured or quietly broken. `Sensitive: true` controls
// display; it does not control persistence. A provider attribute is part of
// the configuration Terraform evaluates, so a secret supplied through one
// reaches disk by ordinary means — written literally into a `.tf`, or passed
// as a variable whose value is recorded in a saved plan produced by
// `terraform plan -out`. Reading the environment inside Configure keeps the
// secret out of every one of those paths, because Terraform never sees it.
//
// So the two secret-bearing inputs have no attributes at all:
//
//	<PREFIX>_CLIENT_ID + <PREFIX>_CLIENT_SECRET   service account (supported)
//	<PREFIX>_API_KEY                              API key (discouraged)
//
// The cost is that a secret cannot be wired from a Terraform variable or a
// vault data source, which is the intended trade: that wiring is exactly the
// path that puts it in a plan file.
func (p *aiProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages k0rdent AI infrastructure through the k0rdent AI API.\n\n" +
			"Credentials are read from the environment and cannot be set here — see the " +
			"provider documentation for why, and for how to obtain a service account.\n\n" +
			"A few values still have to be written as literals, because no read a tenant " +
			"credential can make supplies them. The [hardcoded values](" + HardcodedValuesIndexLink +
			") guide names each known gap and the side that has to change.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Base URL of the k0rdent AI API, for example `" + DefaultEndpoint + "`. " +
					"Falls back to the `" + EnvVar(envEndpoint) + "` environment variable, then to `" +
					DefaultEndpoint + "`, which is the contract's declared server and has not been " +
					"verified against any landscape.",
			},
			"region": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Region id for regional resources, used as the `{region}` URL segment. " +
					"Falls back to the `" + EnvVar(envRegion) + "` environment variable. " +
					"Ids are operator-declared and deployment-specific; read them from `" + regionsOperation + "`, " +
					"where each entry's `id` is this value. It's also the region a new `" +
					ResourceName(projectResourceSuffix) + "` defaults to, so in practice it's the region your " +
					"project lives in. `" + globalRegion + "` is the control-plane tier, which the provider " +
					"addresses on its own for authentication and IAM, and is a region only on an installation " +
					"that runs compute under it, where the regions list reports it with `hosts_projects`. " +
					"Optional, because discovering a valid id requires a configured provider.",
			},
			"project": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Project slug for project-scoped resources, used as the `{project}` URL segment. " +
					"Falls back to the `" + EnvVar(envProject) + "` environment variable. " +
					"This is the project's `id`, as the `" + ResourceName(projectsDataSourceSuffix) + "` data source reports it. " +
					"Optional, because the region-wide catalogs are not project-scoped.",
			},
			"operation_timeout": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "How long to wait for an asynchronous change — a cluster create, a scale, " +
					"a destroy — to settle before giving up. A Go duration string such as `45m`. " +
					"Falls back to the `" + EnvVar(envOperationTimeout) + "` environment variable, then to `" +
					DefaultOperationTimeout.String() + "`.\n\n" +
					"Unset, a cluster's and an instance group's create and delete wait up to `" +
					shortDuration(DefaultLongOperationTimeout) + "` instead, because building or freeing hardware can take " +
					"more than 2 hours. Set, it applies to those too.\n\n" +
					"A wait that runs out stops nothing on the platform. A create that stops waiting fails " +
					"and Terraform taints it: `terraform untaint` and then `terraform apply` wait for it. A " +
					"delete that stops waiting stays in state, and the next `terraform destroy` waits for it.",
			},
			defaultTagsAttribute: schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Tags for every resource this provider manages that carries `tags`. " +
					"Each of those resources shows the result in its computed `tags_all`: these with the " +
					"resource's own `tags` on top, a resource tag winning over a default with the same key. " +
					"Changing one here updates each resource in place, and removing one removes it, except " +
					"on a VPC peering, whose tags the API sets only at creation. A value that isn't known " +
					"until apply is allowed. It leaves `tags_all` unknown in the plan, so every cluster, network " +
					"and security group plans an in-place update, and at apply one whose tags come out the same " +
					"as before sends nothing. A VPC peering plans nothing. The key `" +
					createTokenTag() + "` is the provider's own and is refused.",
				Validators: []validator.Map{reservedTagKey{}},
			},
			"poll_interval": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "How often to re-read a resource while waiting for it to settle. A Go " +
					"duration string such as `10s`. Falls back to the `" + EnvVar(envPollInterval) +
					"` environment variable, then to `" + DefaultPollInterval.String() + "`.",
			},
		},
	}
}

// Configure resolves the provider block against the environment, builds the
// generated client once, and hands it to resources and data sources.
//
// It does nothing else. ADR D4 keeps waiting, polling, retrying and collapsed
// 404s out of the client, and none of that is smuggled in here either: no
// credential probe, no region lookup, no connectivity check. Lane 0c's
// transport already holds the token and shares one mint across concurrent
// calls, so there is no token handling at this layer.
func (p *aiProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var block providerBlock

	// A null configuration object is not the same as a block with null
	// attributes, and decoding it into a struct produces the framework's
	// "this is always an error in the provider" diagnostic. Terraform sends an
	// all-null object rather than a null one for an absent `provider` block,
	// so this is defensive — but the message it prevents accuses the provider
	// of a bug and tells the user to report it, which is a bad way to learn
	// that you have no credential.
	if !req.Config.Raw.IsNull() {
		resp.Diagnostics.Append(req.Config.Get(ctx, &block)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	values, ok := block.values(&resp.Diagnostics)
	if !ok {
		return
	}

	getenv := p.getenv()
	settings, diags := resolveSettings(values, getenv)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	meta, err := newMeta(settings, UserAgent(p.version, req.TerraformVersion, getenv(appendUserAgentEnv)))
	if err != nil {
		// Local: nothing has been sent yet, so there is no upstream code.
		resp.Diagnostics.AddError(
			"Could not build the API client",
			fmt.Sprintf("Endpoint %q: %s", settings.Endpoint, err),
		)
		return
	}

	// Not one of the values above: an unknown here is allowed, and leaves
	// each resource's tags_all unknown until apply rather than failing the
	// plan.
	meta.DefaultTags, diags = defaultTagsFrom(ctx, block.DefaultTags)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	p.sharedDefaults().store(meta.DefaultTags)

	resp.ResourceData = meta
	resp.DataSourceData = meta
	resp.EphemeralResourceData = meta
	resp.ListResourceData = meta
	resp.ActionData = meta
}

// providerBlock mirrors the schema. It is a Terraform configuration model, not
// an API type, so ADR D1 does not reach it — nothing here is serialised to the
// platform.
type providerBlock struct {
	Endpoint         types.String `tfsdk:"endpoint"`
	Region           types.String `tfsdk:"region"`
	Project          types.String `tfsdk:"project"`
	OperationTimeout types.String `tfsdk:"operation_timeout"`
	PollInterval     types.String `tfsdk:"poll_interval"`
	DefaultTags      types.Map    `tfsdk:"default_tags"`
}

// values flattens the block to plain strings, reporting any attribute whose
// value is not known yet.
//
// An unknown here means the attribute was written as a reference to something
// that does not exist until apply, so the provider cannot be built during
// plan. Terraform's own message for that is generic; naming the attribute and
// the environment variable that sidesteps it is the difference between a
// one-minute fix and a bisect.
func (b providerBlock) values(diags *diag.Diagnostics) (blockValues, bool) {
	unknown := func(name, envSuffix string, v types.String) bool {
		if !v.IsUnknown() {
			return false
		}
		diags.AddAttributeError(
			path.Root(name),
			"Unknown provider configuration value",
			fmt.Sprintf(
				"`%s` is not known until apply, so the provider cannot be configured during plan.\n\n"+
					"Either give it a value that is known at plan time, or leave the attribute out and "+
					"set the %s environment variable instead.",
				name, EnvVar(envSuffix),
			),
		)
		return true
	}

	bad := unknown("endpoint", envEndpoint, b.Endpoint)
	bad = unknown("region", envRegion, b.Region) || bad
	bad = unknown("project", envProject, b.Project) || bad
	bad = unknown("operation_timeout", envOperationTimeout, b.OperationTimeout) || bad
	bad = unknown("poll_interval", envPollInterval, b.PollInterval) || bad
	if bad {
		return blockValues{}, false
	}

	return blockValues{
		Endpoint:         b.Endpoint.ValueString(),
		Region:           b.Region.ValueString(),
		Project:          b.Project.ValueString(),
		OperationTimeout: b.OperationTimeout.ValueString(),
		PollInterval:     b.PollInterval.ValueString(),
	}, true
}

// Resources returns every managed resource.
//
// Each entry's Metadata names itself with ResourceName("<suffix>"), never a
// literal. That is the rule the name audit enforces: the provisional prefix
// lives in one constant, so settling the name is an edit to that constant rather
// than a sweep through every resource file.
func (p *aiProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newClusterResource,
		newInstanceGroupResource,
		newNetworkResource,
		newSubnetResource,
		newSecurityGroupResource,
		vpcPeeringResourceWith(p.sharedDefaults()),
		newVPCSecurityGroupsResource,
		newFileStoreResource,
		newProjectResource,
		newWebhookSubscriptionResource,
	}
}

// EphemeralResources returns every ephemeral resource. Secret-bearing surfaces
// live here: nothing in this list is written to state.
func (p *aiProvider) EphemeralResources(_ context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{
		newClusterKubeconfigEphemeral,
	}
}

// ListResources returns every list resource, one for each managed resource
// whose platform type has a list operation a tenant can call. Each shares its
// managed resource's name, which is how Terraform pairs a `list` block's
// results with the resource and identity schemas they're written in.
func (p *aiProvider) ListResources(_ context.Context) []func() list.ListResource {
	return listResources()
}

// Actions returns every action. An action runs only when a configuration
// invokes it by name or binds it with `action_trigger`, never as part of an
// ordinary plan or apply, which is what makes it the place for a side effect
// such as cancelling an operation. Actions need Terraform 1.14, and a
// configuration that uses none runs on older versions as before.
func (p *aiProvider) Actions(_ context.Context) []func() action.Action {
	return []func() action.Action{
		newClusterCancelOperationAction,
	}
}

// DataSources returns every data source.
func (p *aiProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newRegionsDataSource,
		newProjectsDataSource,
		newProjectDataSource,
		newServiceAccountDataSource,
		newServiceAccountsDataSource,
		newServiceAccountCredentialsDataSource,
		newRolesDataSource,
		newOrganizationsDataSource,
		newOrganizationDataSource,
		newClusterDataSource,
		newInstanceGroupDataSource,
		newInstancesDataSource,
		newInstanceDataSource,
		newFileStoreDataSource,
		newNetworkDataSource,
		newSubnetDataSource,
		newSecurityGroupDataSource,
		newVPCPeeringDataSource,
		newClusterTypesDataSource,
		newClusterTypeDataSource,
		newClusterTypeVersionsDataSource,
		newClusterTypeVersionDataSource,
		newClusterKubeconfigDataSource,
		newVPCsDataSource,
		newVPCDataSource,
		newTenantQuotaDataSource,
		newProjectQuotaDataSource,
		newNetworksDataSource,
		newSubnetsDataSource,
		newSecurityGroupsDataSource,
		newEffectiveSecurityGroupsDataSource,
		newVPCPeeringsDataSource,
		newAddressPoolsDataSource,
		newAddressPoolDataSource,
		newShareTypesDataSource,
		newShareTypeDataSource,
		newWebhookSubscriptionDataSource,
		newWebhookSubscriptionsDataSource,
		newWebhookDeliveriesDataSource,
	}
}
