package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*clusterKubeconfigDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*clusterKubeconfigDataSource)(nil)
)

const kubeconfigDataSourceSuffix = "cluster_kubeconfig"

func newClusterKubeconfigDataSource() datasource.DataSource { return &clusterKubeconfigDataSource{} }

type clusterKubeconfigDataSource struct{ meta *Meta }

type clusterKubeconfigModel struct {
	Region    types.String `tfsdk:"region"`
	Project   types.String `tfsdk:"project"`
	ClusterID types.String `tfsdk:"cluster_id"`
	ID        types.String `tfsdk:"id"`
	IssuedFor types.String `tfsdk:"issued_for"`
	Status    types.String `tfsdk:"status"`
	Available types.Int64  `tfsdk:"available"`
}

func (d *clusterKubeconfigDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(kubeconfigDataSourceSuffix)
}

// Schema declares the data source.
//
// # Why this is a data source and not a resource
//
// **There is no operation that creates a kubeconfig.** The published contract
// has exactly two, `listKubeconfigs` and `downloadKubeconfig`, and it is not a
// visibility artifact: there are zero matches for a create across all 264
// operations in the raw bundle, internal half included. So this
// provider can only hand back a credential something else already issued. A
// managed resource would have to promise a create it cannot make.
//
// If issuance lands upstream, this can become a managed resource.
//
// # Why there is no body here
//
// A kubeconfig is a credential, and everything a data source returns is written
// to state in plain text. This reads metadata only; the credential comes from
// the ephemeral resource of the same name, which never persists.
func (d *clusterKubeconfigDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Facts about a cluster's kubeconfig: which one, and whether it is " +
			"usable. For the kubeconfig itself use the ephemeral resource of the same name.\n\n" +
			"The platform issues a cluster's kubeconfig shortly after the cluster becomes active. " +
			"On an active cluster this waits up to two minutes for that, within the provider's " +
			"`operation_timeout`. On a cluster still creating or updating, or `failed`, it fails at " +
			"once rather than hold a plan for the length of a provisioning.\n\n" +
			"For a cluster created in the same apply, `cluster_id = <cluster>.id` is enough: the " +
			"reference makes this depend on the cluster, and Terraform holds the read until the " +
			"cluster's create has applied. A `depends_on` on the cluster does the same for a cluster " +
			"named some other way.",
		Attributes: map[string]schema.Attribute{
			"cluster_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the cluster whose kubeconfigs to read.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"region": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Region the cluster is in. Defaults to the provider's `region`.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"project": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Project the cluster is in. Defaults to the provider's `project`.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			// Optional+Computed with no drift to lose: a data source is read
			// every plan and manages nothing. Omitted, it names whichever
			// kubeconfig was chosen.
			"id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Which kubeconfig. Omit it to take the first `active` one the " +
					"platform lists.",
				Validators: []validator.String{urlSafeIdentifier{}},
			},
			"issued_for": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Who the stored kubeconfig record names. The record grants nothing " +
					"itself: the token in a kubeconfig is minted for whoever reads it.",
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Lifecycle status: `active` is usable, `failed` is terminal. " +
					"The platform's list excludes `pending`, `revoking` and `revoked`.",
			},
			"available": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "How many kubeconfig records the platform lists for this cluster, " +
					"including earlier attempts that failed. One of them is usable.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *clusterKubeconfigDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *clusterKubeconfigDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data clusterKubeconfigModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	clusterID := data.ClusterID.ValueString()

	items, diags := listKubeconfigs(ctx, d.meta, target, clusterID)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.Available = types.Int64Value(int64(len(items)))

	chosen, diags := pickKubeconfig(items, data.ID, clusterID)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.ID = stringOrNull(chosen.Id)
	data.IssuedFor = stringOrNull(chosen.IssuedFor)
	data.Status = types.StringNull()
	if chosen.Status != nil {
		data.Status = types.StringValue(string(*chosen.Status))
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// listKubeconfigs reads the cluster's kubeconfig records, waiting while an
// active cluster's kubeconfig is still being issued.
//
// It does not wait for a cluster that is still provisioning. A data source with
// a known cluster id is read during plan, and blocking a plan for the length of
// a provisioning is worse than saying so. In one apply the read comes after the
// cluster resource has waited for `active`, so this wait covers that case.
func listKubeconfigs(ctx context.Context, meta *Meta, target clusterTarget, clusterID string) ([]k0rdent.Kubeconfig, diag.Diagnostics) {
	var diags diag.Diagnostics
	var items []k0rdent.Kubeconfig
	cfg := meta.Wait
	cfg.Raise = "`operation_timeout` in the provider block"
	err := waitFor(ctx, cfg, fmt.Sprintf("the kubeconfig of cluster %q", clusterID), func(ctx context.Context) (bool, error) {
		listed, err := readKubeconfigList(ctx, meta, target, clusterID)
		if err != nil {
			return false, unlessDeadline(ctx, err)
		}
		// The list leaves out a `pending` issuance, so empty is the same
		// transient state the canonical read answers 404 for.
		if len(listed) > 0 {
			items = listed
			return true, nil
		}
		return false, kubeconfigIsComing(ctx, meta, target, clusterID, false)
	})
	if err != nil {
		addKubeconfigError(&diags, "Could not list the cluster's kubeconfigs", err)
		return nil, diags
	}
	return items, diags
}

// downloadKubeconfig fetches the kubeconfig body, waiting out the 404 the
// contract calls transient while the platform creates a kubeconfig for a newly
// active cluster. A cluster still creating is waited on too, because the
// provider that consumes this needs the credential, not an error.
func downloadKubeconfig(ctx context.Context, meta *Meta, target clusterTarget, clusterID string) (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	var raw string
	cfg := meta.Wait
	cfg.Raise = "`timeouts.open` on this ephemeral resource, or `operation_timeout` in the provider block"
	err := waitFor(ctx, cfg, fmt.Sprintf("the kubeconfig of cluster %q", clusterID), func(ctx context.Context) (bool, error) {
		body, issued, err := fetchKubeconfig(ctx, meta, target, clusterID)
		if err != nil || issued {
			raw = body
			return issued, err
		}
		return false, kubeconfigIsComing(ctx, meta, target, clusterID, true)
	})
	if err != nil {
		addKubeconfigError(&diags, "Could not download the kubeconfig", err)
		return "", diags
	}
	return raw, diags
}

// fetchKubeconfig makes one read. issued is false on the 404 that means the
// cluster has none yet, or no longer exists. The caller tells those apart.
func fetchKubeconfig(ctx context.Context, meta *Meta, target clusterTarget, clusterID string) (string, bool, error) {
	// The unparsed operation on purpose. `getClusterKubeconfig` is declared as
	// a string returning `application/yaml`, and the generated parser rejects
	// every real response before a caller sees it, so the typed call fails on a
	// body that arrived perfectly intact.
	rsp, err := meta.Client.GetClusterKubeconfig(ctx, target.region, target.project, clusterID)
	if err != nil {
		return "", false, unlessDeadline(ctx, fmt.Errorf("the request did not complete: %w", err))
	}
	defer func() { _ = rsp.Body.Close() }()

	body, err := io.ReadAll(rsp.Body)
	if err != nil {
		return "", false, unlessDeadline(ctx, fmt.Errorf("the response could not be read: %w", err))
	}
	switch {
	case rsp.StatusCode == http.StatusOK:
		if len(body) == 0 {
			return "", false, kubeconfigFailure{
				summary: "The kubeconfig download was empty",
				detail:  fmt.Sprintf("The platform answered 200 for the kubeconfig of cluster %q with no body.", clusterID),
			}
		}
		return string(body), true, nil
	case rsp.StatusCode/100 == 2:
		// Not quoted: on this operation the body is the credential, and
		// redactDiagnosticText knows the provider's own secrets, not this one.
		return "", false, kubeconfigFailure{
			summary: "Could not download the kubeconfig",
			detail: fmt.Sprintf("The platform answered %s for the kubeconfig of cluster %q. The body of "+
				"this operation is the credential, so it is not quoted here.", rsp.Status, clusterID),
		}
	case isNotFound(rsp, body):
		return "", false, nil
	case rsp.StatusCode == http.StatusNotFound:
		return "", false, kubeconfigFailure{
			summary: "The platform has no route for the kubeconfig read",
			detail: "This landscape doesn't serve the kubeconfig read this provider uses, and " +
				"waiting won't change that.",
			api: apiErrorOf(rsp, body),
		}
	case rsp.StatusCode == http.StatusServiceUnavailable:
		// Not waited on past the transport's bounded retry of a GET's 503. The
		// platform answers 503 while its identity service is down, and also,
		// permanently, for a caller it will not mint for.
		return "", false, kubeconfigFailure{
			summary: "The platform could not mint a token for the kubeconfig",
			detail: "The platform answers this while it cannot mint tokens, and also for a caller it " +
				"will not mint for, such as one with no membership in the cluster's organization. " +
				"The two look the same, so this is reported rather than waited on.",
			api: apiErrorOf(rsp, body),
		}
	default:
		return "", false, responseError(rsp, body)
	}
}

// kubeconfigIsComing reads the cluster after a read found no kubeconfig, and
// returns nil when one is still to be issued. A missing or deleting cluster
// will never have one, so those end the wait at once rather than at the
// timeout.
func kubeconfigIsComing(ctx context.Context, meta *Meta, target clusterTarget, clusterID string, waitForProvisioning bool) error {
	got, err := meta.Client.GetClusterWithResponse(ctx, target.region, target.project, clusterID, nil)
	if err != nil {
		return unlessDeadline(ctx, fmt.Errorf("the request did not complete: %w", err))
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		return kubeconfigFailure{
			summary: "The cluster does not exist",
			detail: fmt.Sprintf("cluster %q does not exist in project %q of region %q, so it has no kubeconfig.",
				clusterID, target.project, target.region),
			api: apiErrorOf(got.HTTPResponse, got.Body),
		}
	}
	if got.JSON200 == nil {
		return responseError(got.HTTPResponse, got.Body)
	}
	if got.JSON200.State == nil {
		return fmt.Errorf("the platform returned cluster %q with no state, which the contract requires", clusterID)
	}
	state := *got.JSON200.State
	notYet := kubeconfigFailure{
		summary: "The cluster has no kubeconfig yet",
		detail: fmt.Sprintf("Cluster %q is %s. The platform issues a kubeconfig once a cluster is active. "+
			"This data source is read during plan and does not wait for a cluster to provision. "+
			"The ephemeral resource of the same name does.", clusterID, state),
	}
	switch kubeconfigOutlookFor(state) {
	case kubeconfigIssuing:
		// The provisional window bounds a wait that is not issuance.
		return provisionalFailure{notIssued(ctx, meta, target, clusterID, waitForProvisioning)}
	case kubeconfigAfterProvisioning:
		if waitForProvisioning {
			return nil
		}
		return notYet
	case kubeconfigNotWhileFailed:
		failed := fmt.Errorf("the platform reports cluster %q as failed%s, and issues a kubeconfig only "+
			"to an active cluster", clusterID, stateDetail(got.JSON200.StateMessage, reasonString(got.JSON200.StateReason)))
		if waitForProvisioning {
			return provisionalFailure{failed}
		}
		return failed
	case kubeconfigNever:
		return kubeconfigFailure{
			summary: "The cluster is being deleted",
			detail:  fmt.Sprintf("Cluster %q is %s, so the platform will not issue it a kubeconfig.", clusterID, state),
		}
	default:
		return unknownStateError("cluster", string(state))
	}
}

// notIssued says an active cluster has no kubeconfig. When asked, it reads the
// list for a `failed` issue and quotes its reason. It does not end the wait on
// one: the list keeps earlier failed attempts and hides a `pending` one, so a
// failed entry can sit beside an issue that is about to succeed.
func notIssued(ctx context.Context, meta *Meta, target clusterTarget, clusterID string, readReason bool) error {
	failure := kubeconfigFailure{
		summary: "The platform has not issued the cluster a kubeconfig",
		detail: fmt.Sprintf("cluster %q is active and the platform has not issued it a kubeconfig. "+
			"It issues one when a cluster settles, and does not retry a failed issue until the "+
			"cluster's next lifecycle change.", clusterID),
	}
	if !readReason {
		return failure
	}
	listed, err := readKubeconfigList(ctx, meta, target, clusterID)
	if err != nil {
		// Only the reason is lost, so the list's own failure is not reported.
		return failure
	}
	for _, k := range listed {
		if k.Status != nil && *k.Status == k0rdent.KubeconfigStatusFailed && k.FailureReason != nil && *k.FailureReason != "" {
			failure.detail += fmt.Sprintf(" The last issue failed: %s.", *k.FailureReason)
			break
		}
	}
	return failure
}

// readKubeconfigList reads every page of the cluster's kubeconfig records. It
// returns an error rather than a diagnostic because both callers read it inside
// a wait, which decides what a failure means.
func readKubeconfigList(ctx context.Context, meta *Meta, target clusterTarget, clusterID string) ([]k0rdent.Kubeconfig, error) {
	return byToken(catalog, "kubeconfigs", func(token *string, size *int) (page[k0rdent.Kubeconfig], error) {
		// Deprecated upstream with no replacement: the canonical read returns
		// the body alone, and this is the only operation that reports the
		// metadata.
		listed, err := meta.Client.ListKubeconfigsWithResponse(ctx, target.region, target.project, clusterID, //nolint:staticcheck
			&k0rdent.ListKubeconfigsParams{PageToken: token, PageSize: size})
		if err != nil {
			return page[k0rdent.Kubeconfig]{}, fmt.Errorf("the request did not complete: %w", err)
		}
		if listed.JSON200 == nil {
			return page[k0rdent.Kubeconfig]{}, responseError(listed.HTTPResponse, listed.Body)
		}
		return page[k0rdent.Kubeconfig]{items: listed.JSON200.Kubeconfigs, next: listed.JSON200.NextPageToken}, nil
	})
}

// unlessDeadline returns err, except for a failure caused by the wait's own
// deadline, which it drops so the wait reports its timeout rather than an I/O
// error that hides it.
func unlessDeadline(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil //nolint:nilerr // waitFor sees the same deadline and reports it
	}
	return err
}

// kubeconfigFailure is an error that says what it means in its own words, with
// the platform's answer attached when there was one.
type kubeconfigFailure struct {
	summary, detail string
	api             *k0rdent.APIError
}

func (f kubeconfigFailure) Error() string { return f.detail }

func addKubeconfigError(diags *diag.Diagnostics, summary string, err error) {
	var f kubeconfigFailure
	if !errors.As(err, &f) {
		addWaitError(diags, summary, err)
		return
	}
	detail := f.detail
	if f.api != nil {
		detail += "\n\n" + apiErrorDetail(f.api)
	}
	diags.AddError(f.summary, redactDiagnosticText(detail))
}

// responseError is a response the caller cannot use, as an error that keeps
// the platform's code, message and request id.
func responseError(resp *http.Response, body []byte) error {
	if err := k0rdent.CheckResponse(resp, body); err != nil {
		return err
	}
	return newAnswerError(resp, ", which this provider does not know how to read")
}

// pickKubeconfig chooses which listed kubeconfig to report.
func pickKubeconfig(items []k0rdent.Kubeconfig, want types.String, clusterID string) (k0rdent.Kubeconfig, diag.Diagnostics) {
	var diags diag.Diagnostics

	if len(items) == 0 {
		diags.AddError(
			"The cluster has no kubeconfig",
			fmt.Sprintf("The platform lists no kubeconfig for %q. One is created when the "+
				"cluster becomes active, so a cluster still provisioning has none yet.", clusterID),
		)
		return k0rdent.Kubeconfig{}, diags
	}

	if !want.IsNull() && !want.IsUnknown() && want.ValueString() != "" {
		id := want.ValueString()
		for _, k := range items {
			if k.Id != nil && *k.Id == id {
				return k, diags
			}
		}
		diags.AddError(
			"No such kubeconfig on the cluster",
			fmt.Sprintf("Cluster %q lists %d kubeconfig(s), none with id %q.", clusterID, len(items), id),
		)
		return k0rdent.Kubeconfig{}, diags
	}

	for _, k := range items {
		if k.Status != nil && *k.Status == k0rdent.KubeconfigStatusActive {
			return k, diags
		}
	}

	// Nothing active. Report the first anyway rather than failing: the status is
	// in state, so a configuration can see `failed` and act, and an error here
	// would be less informative than the status itself.
	diags.AddWarning(
		"No active kubeconfig on the cluster",
		fmt.Sprintf("Cluster %q lists %d kubeconfig(s) and none is `active`. Reporting the first. "+
			"The data source of this name reports each one's `status`.", clusterID, len(items)),
	)
	return items[0], diags
}
