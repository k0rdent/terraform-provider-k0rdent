package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/ephemeral/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ ephemeral.EphemeralResource              = (*clusterKubeconfigEphemeral)(nil)
	_ ephemeral.EphemeralResourceWithConfigure = (*clusterKubeconfigEphemeral)(nil)
	_ ephemeral.EphemeralResourceWithClose     = (*clusterKubeconfigEphemeral)(nil)
)

func newClusterKubeconfigEphemeral() ephemeral.EphemeralResource {
	return &clusterKubeconfigEphemeral{}
}

type clusterKubeconfigEphemeral struct{ meta *Meta }

type clusterKubeconfigEphemeralModel struct {
	Region    types.String   `tfsdk:"region"`
	Project   types.String   `tfsdk:"project"`
	ClusterID types.String   `tfsdk:"cluster_id"`
	Raw       types.String   `tfsdk:"raw"`
	Timeouts  timeouts.Value `tfsdk:"timeouts"`
	Relay     types.Bool     `tfsdk:"relay"`

	Host                 types.String `tfsdk:"host"`
	ClusterCACertificate types.String `tfsdk:"cluster_ca_certificate"`
	Token                types.String `tfsdk:"token"`
	ClientCertificate    types.String `tfsdk:"client_certificate"`
	ClientKey            types.String `tfsdk:"client_key"`
}

func (e *clusterKubeconfigEphemeral) Metadata(_ context.Context, _ ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = ResourceName(kubeconfigDataSourceSuffix)
}

// A kubeconfig is a credential, and everything a data source or resource returns
// is written to state in plain text. `Sensitive: true` hides it from the console,
// not from the file. An ephemeral resource is the only surface that never
// persists, so this is where the credential itself belongs; the data source next
// to it stays for reading metadata.
//
// There is no Renew. Terraform's renewal extends a lease on the value it already
// handed out, and RenewResponse carries no new result, so a fresh token could
// never reach the provider configured from the old one. `relay` handles that.
func (e *clusterKubeconfigEphemeral) Schema(ctx context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A kubeconfig for a cluster, fetched for the length of the run and " +
			"never written to state.\n\n" +
			"For a cluster created in the same apply, pass `cluster_id = <cluster>.state != null ? " +
			"<cluster>.id : null`. Terraform opens an ephemeral resource during the plan whenever its " +
			"arguments are known, `depends_on` or not, and the cluster's `id` is known from configuration, " +
			"so plain `.id` opens it before the cluster exists. `state` is unknown until the create runs, " +
			"which holds the open until the apply.\n\n" +
			"The token in it is minted for the caller when this opens, carries the caller's own " +
			"roles (an API key gets the key's roles, not its owner's) and is short-lived (minutes " +
			"rather than hours). The platform can't renew it, and Terraform can't hand a new one to a " +
			"kubernetes or helm provider it has already configured. Its lifetime starts when this opens, " +
			"which in the apply that creates the cluster is as soon as the cluster is active, not when the " +
			"in-cluster work starts. So without `relay`, in-cluster work that runs past it fails with " +
			"`401 Unauthorized`, and the next run opens a new one.\n\n" +
			"With `relay = true`, this provider runs a proxy on `127.0.0.1` for the length of the run, " +
			"and `host`, `cluster_ca_certificate`, `token` and `raw` point at it. It sends each request " +
			"to the cluster with the platform's current token, so the kubernetes and helm providers keep " +
			"working after the first token expires. Set it when in-cluster work can run past the token, " +
			"such as a long Helm install or a rollout wait. It needs nothing installed on the cluster or " +
			"this machine.\n\n" +
			"A minute before the token expires (a quarter of its lifetime for a token under 4 minutes), " +
			"the next request starts a fetch of a new kubeconfig. Requests keep the current token until " +
			"the new one arrives. Only a request after the token has expired waits.\n\n" +
			"If the API server still refuses the token with `401`, the relay fetches once and resends " +
			"the request. It doesn't resend when the `401` could come after the request ran (from a pod, " +
			"service or node behind a `proxy` path, or from an admission webhook), when the body is over " +
			"4 MiB, or when the token was fetched after a `401` less than a minute ago.\n\n" +
			"If the platform won't issue a new kubeconfig, for example because the credential was " +
			"revoked, the request fails with `502` and the platform's reason. A fetch has the open's time " +
			"limit (`timeouts.open` or `operation_timeout`), and fetches are at least 10 seconds apart, " +
			"or a quarter of the token's lifetime when that's shorter.\n\n" +
			"The kubernetes and helm providers work through the relay, and so do `kubectl` and `helm` " +
			"run from `local-exec`, including watches, `exec`, `attach` and `port-forward`. The relay " +
			"connects with the kubeconfig's CA, client certificate, `tls-server-name` and `proxy-url` " +
			"(else `HTTPS_PROXY` and `NO_PROXY`), and always checks the cluster's certificate, even with " +
			"`insecure-skip-tls-verify`.\n\n" +
			"The relay works only on the machine running Terraform. A container started from " +
			"`local-exec`, or anything run by `remote-exec`, can't reach it. It stops when Terraform " +
			"closes this resource, and each plan and apply starts a new one, so a kubeconfig saved from " +
			"`raw` stops working after the run. A kubeconfig with only a client certificate isn't " +
			"relayed: the result connects to the cluster directly, with a warning.\n\n" +
			"The relay listens only on `127.0.0.1`, over TLS with a CA made for the run, and accepts " +
			"only requests carrying `token`, a random secret made for the run. The platform's token " +
			"never leaves this provider, and neither it nor the secret is logged. During the run, any " +
			"process on this machine holding `token` can reach the cluster with your roles.\n\n" +
			"`host`, `cluster_ca_certificate` and `token` are the kubeconfig's current context, parsed " +
			"and named as the kubernetes and helm providers name their own arguments, so each goes " +
			"into those providers' configuration as it is. With no `current-context`, a kubeconfig " +
			"with exactly one context uses that one, a leniency kubectl doesn't share.\n\n" +
			"The parse fails closed and never fails the open. When the context can't be chosen " +
			"(several and none current, or one that names a cluster or user the file lacks), or the " +
			"user carries no credential this provider hands over (an exec plugin, an auth provider, " +
			"a token or certificate file), every parsed attribute is null, `host` included, and " +
			"Terraform shows a warning that says what it found. A null `host` keeps a local credential from being sent " +
			"to this cluster, but it doesn't stop the kubernetes and helm providers: given a null " +
			"`host` they fall back to `KUBE_HOST`, `KUBE_TOKEN`, `KUBE_CONFIG_PATH(S)`, `KUBE_CTX` and " +
			"the rest, and reach whatever cluster those name. To stop the run instead, give this " +
			"block `lifecycle { postcondition { condition = self.host != null && self.token != null } }` " +
			"(with an `error_message`). For an exec plugin, wire the kubernetes provider's own `exec` " +
			"block from `yamldecode(raw)`. `raw` is set whatever the parse makes of it.\n\n" +
			"The platform issues a cluster's kubeconfig shortly after the cluster becomes active, so " +
			"this waits while the cluster is creating or updating, bounded by `timeouts.open` or the " +
			"provider's `operation_timeout`. When `cluster_id` is known at plan time this opens during " +
			"the plan, so that bound holds a plan too.\n\n" +
			"An active cluster with no kubeconfig, and a `failed` cluster, are tolerated for two " +
			"minutes and then reported. It fails at once when " +
			"the cluster does not exist or is being deleted, when the platform cannot mint a token, " +
			"and when the read answers 404 without the service's error envelope, because then the " +
			"request didn't reach the service.\n\n" +
			"When the cluster's API answers `401 Unauthorized` to this kubeconfig, the kubernetes or " +
			"helm provider reports it. Without `relay`, this provider never sees it: a run that outlives " +
			"the token gets that answer, and running again opens a new one. A `401` on a fresh token: report it to your platform " +
			"operator with the cluster's id and the time.\n\n" +
			"It can't be imported, because there's nothing to import into: an ephemeral resource " +
			"keeps no state. Open it again in each run instead.",
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx),
		},
		Attributes: map[string]schema.Attribute{
			"cluster_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The cluster to fetch a kubeconfig for.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			// A cluster can be created outside the provider's scope, so the
			// credential has to be reachable there too.
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
			"relay": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Reach the cluster through a relay on `127.0.0.1` that keeps the token " +
					"current for the whole run. Off when unset. When on and the kubeconfig carries a token, " +
					"`host`, `cluster_ca_certificate`, `token` and `raw` point at the relay, and " +
					"`client_certificate` and `client_key` are null.",
			},
			"raw": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The kubeconfig YAML. With `relay`, a kubeconfig for the relay. It keeps the " +
					"platform's cluster, context and user names, for `kubectl` and `helm` in `local-exec`.",
			},
			// The connection, parsed once here and named as the kubernetes and
			// helm providers name their own configuration, so a configuration
			// hands each one over directly instead of decoding the YAML itself
			// and indexing into whichever entry happens to come first.
			"host": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The API server URL of the kubeconfig's current context, for the " +
					"`host` of the kubernetes and helm providers. Null whenever there's no credential to go " +
					"with it. Those providers fall back to `KUBE_HOST` and the other `KUBE_*` variables when " +
					"it's null, so a postcondition that it isn't is what keeps them off another cluster. " +
					"With `relay`, the relay's `https://127.0.0.1:<port>`, which works only on this machine.",
			},
			"cluster_ca_certificate": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The cluster's CA certificate, PEM-encoded and ready for the " +
					"`cluster_ca_certificate` of the kubernetes and helm providers, with no `base64decode`. " +
					"Null when the kubeconfig carries no CA, and then those providers trust the system roots. " +
					"Null too whenever `host` is. With `relay`, the certificate authority made for the relay.",
			},
			"token": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The bearer token of the current context's user, for the `token` " +
					"of the kubernetes and helm providers. This is the credential the platform mints, so " +
					"it carries the same short lifetime. With `relay`, the relay's secret instead, which " +
					"works until the run ends. Null when the user carries none.",
			},
			"client_certificate": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The current context's client certificate, PEM-encoded, for the " +
					"`client_certificate` of the kubernetes and helm providers. Null unless the kubeconfig " +
					"authenticates with a client certificate. The platform's kubeconfig carries a token instead. " +
					"Null with `relay`, which presents the certificate itself.",
			},
			"client_key": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The private key of `client_certificate`, PEM-encoded, for the " +
					"`client_key` of the kubernetes and helm providers. Null whenever `client_certificate` is.",
			},
		},
	}
	documentEphemeralSchema(&resp.Schema)
}

func (e *clusterKubeconfigEphemeral) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	e.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (e *clusterKubeconfigEphemeral) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var data clusterKubeconfigEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(e.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	clusterID := data.ClusterID.ValueString()

	ctx, cancel := withTimeout(ctx, data.Timeouts.Open, e.meta.Wait.Timeout, &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	raw, diags := downloadKubeconfig(ctx, e.meta, target, clusterID)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.Raw = types.StringValue(raw)
	conn, warnings := parseKubeconfigConnectionFor(raw, data.Relay.ValueBool())
	for _, w := range warnings {
		resp.Diagnostics.AddWarning("The kubeconfig's connection is incomplete", redactDiagnosticText(w))
	}
	data.Host = emptyAsNull(conn.Host)
	data.ClusterCACertificate = emptyAsNull(conn.ClusterCACertificate)
	data.Token = emptyAsNull(conn.Token)
	data.ClientCertificate = emptyAsNull(conn.ClientCertificate)
	data.ClientKey = emptyAsNull(conn.ClientKey)

	if data.Relay.ValueBool() {
		// Terraform calls Close only after a successful open.
		if id := e.openRelay(ctx, &data, conn, target, clusterID, resp); id != "" {
			defer func() {
				if resp.Diagnostics.HasError() {
					e.closeRelay(id)
				}
			}()
		}
		if resp.Diagnostics.HasError() {
			return
		}
	}

	resp.Diagnostics.Append(resp.Result.Set(ctx, &data)...)
}

const relayPrivateKey = "relay"

// openRelay points the result at a new relay and returns its id, or "" when
// there's no token to relay or the kubeconfig couldn't be parsed.
func (e *clusterKubeconfigEphemeral) openRelay(ctx context.Context, data *clusterKubeconfigEphemeralModel,
	conn kubeconfigConnection, target clusterTarget, clusterID string, resp *ephemeral.OpenResponse) string {
	switch {
	case conn.Host == "":
		return ""
	case conn.Token == "":
		resp.Diagnostics.AddWarning("The kubeconfig is not relayed",
			fmt.Sprintf("The kubeconfig of cluster %q uses a client certificate and no token, so the "+
				"relay has nothing to renew. The result connects to the cluster directly.", clusterID))
		return ""
	}

	meta := e.meta
	fetch := func(ctx context.Context) (kubeconfigConnection, error) {
		raw, diags := downloadKubeconfig(ctx, meta, target, clusterID)
		if diags.HasError() {
			d := diags.Errors()[0]
			return kubeconfigConnection{}, fmt.Errorf("%s: %s", d.Summary(), d.Detail())
		}
		next, warnings := parseKubeconfigConnection(raw)
		if next.Token == "" {
			why := "The new kubeconfig carries no token."
			if len(warnings) > 0 {
				why = warnings[0]
			}
			return kubeconfigConnection{}, errors.New(redactDiagnosticText(why))
		}
		return next, nil
	}
	fetchTimeout, diags := data.Timeouts.Open(ctx, meta.Wait.Timeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return ""
	}
	if fetchTimeout <= 0 {
		fetchTimeout = meta.Wait.Timeout
	}

	relay, err := startKubeconfigRelay(ctx, conn, fetch, fetchTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Could not start the kubeconfig relay",
			fmt.Sprintf("The relay for cluster %q didn't start: %s. Set `relay = false` to connect "+
				"directly.", clusterID, redactDiagnosticText(err.Error())))
		return ""
	}
	raw, err := relayKubeconfig(currentKubeconfigNames(data.Raw.ValueString()), relay.host, relay.caPEM, relay.secret)
	if err != nil {
		relay.close()
		resp.Diagnostics.AddError("Could not start the kubeconfig relay",
			"The kubeconfig for the relay could not be written: "+err.Error())
		return ""
	}
	id, err := meta.relays.add(relay)
	if err != nil {
		relay.close()
		resp.Diagnostics.AddError("Could not start the kubeconfig relay", err.Error())
		return ""
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, relayPrivateKey, []byte(strconv.Quote(id)))...)

	data.Raw = types.StringValue(raw)
	data.Host = types.StringValue(relay.host)
	data.ClusterCACertificate = types.StringValue(relay.caPEM)
	data.Token = types.StringValue(relay.secret)
	data.ClientCertificate = types.StringNull()
	data.ClientKey = types.StringNull()
	return id
}

// Close stops the relay Open started, if any.
func (e *clusterKubeconfigEphemeral) Close(ctx context.Context, req ephemeral.CloseRequest, resp *ephemeral.CloseResponse) {
	b, diags := req.Private.GetKey(ctx, relayPrivateKey)
	resp.Diagnostics.Append(diags...)
	if len(b) == 0 || e.meta == nil {
		return
	}
	id, err := strconv.Unquote(string(b))
	if err != nil {
		return
	}
	e.closeRelay(id)
}

func (e *clusterKubeconfigEphemeral) closeRelay(id string) {
	if relay := e.meta.relays.remove(id); relay != nil {
		relay.close()
	}
}
