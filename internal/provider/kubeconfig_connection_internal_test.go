package provider

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	testCAPEM   = "-----BEGIN CERTIFICATE-----\nNOT-A-REAL-CA\n-----END CERTIFICATE-----\n"
	testCertPEM = "-----BEGIN CERTIFICATE-----\nNOT-A-REAL-CERT\n-----END CERTIFICATE-----\n"
	testKeyPEM  = "-----BEGIN PRIVATE KEY-----\nNOT-A-REAL-KEY\n-----END PRIVATE KEY-----\n"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// twoContexts is a kubeconfig with two clusters and two users, listed so that
// the one current-context names is second. Taking the first entry of each
// list, which is what the example used to do with yamldecode, gets the wrong
// cluster.
var twoContexts = `apiVersion: v1
kind: Config
clusters:
- name: other
  cluster:
    server: https://other.invalid:6443
- name: training
  cluster:
    server: https://training.invalid:6443
    certificate-authority-data: ` + b64(testCAPEM) + `
contexts:
- name: other
  context: {cluster: other, user: other-user}
- name: training
  context: {cluster: training, user: training-user}
current-context: training
users:
- name: other-user
  user: {token: other-token}
- name: training-user
  user: {token: training-token}
`

func TestTheConnectionComesFromTheCurrentContext(t *testing.T) {
	t.Parallel()
	got, warnings := parseKubeconfigConnection(twoContexts)
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	want := kubeconfigConnection{
		Host: "https://training.invalid:6443", ClusterCACertificate: testCAPEM, Token: "training-token",
	}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

// The shape the fake serves: a token, a server, no CA.
func TestATokenKubeconfigWithNoCAHasNoCA(t *testing.T) {
	t.Parallel()
	got, warnings := parseKubeconfigConnection(`apiVersion: v1
kind: Config
clusters:
- name: c1
  cluster: {server: "https://c1.fake.invalid:6443"}
contexts:
- name: c1
  context: {cluster: c1, user: fake-admin}
current-context: c1
users:
- name: fake-admin
  user: {token: not-a-real-token}
`)
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	want := kubeconfigConnection{Host: "https://c1.fake.invalid:6443", Token: "not-a-real-token"}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestAClientCertificateKubeconfigHandsOverPEM(t *testing.T) {
	t.Parallel()
	got, warnings := parseKubeconfigConnection(`clusters:
- name: c
  cluster: {server: "https://c.invalid", certificate-authority-data: ` + b64(testCAPEM) + `}
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user:
    client-certificate-data: ` + b64(testCertPEM) + `
    client-key-data: ` + b64(testKeyPEM) + `
`)
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	want := kubeconfigConnection{
		Host: "https://c.invalid", ClusterCACertificate: testCAPEM,
		ClientCertificate: testCertPEM, ClientKey: testKeyPEM,
	}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

// With no current-context, a file with one context has only one answer.
func TestASingleContextIsTakenWithoutACurrentContext(t *testing.T) {
	t.Parallel()
	got, warnings := parseKubeconfigConnection(`clusters:
- name: c
  cluster: {server: "https://c.invalid"}
contexts:
- name: c
  context: {cluster: c, user: u}
users:
- name: u
  user: {token: t}
`)
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if got.Host != "https://c.invalid" || got.Token != "t" {
		t.Errorf("got %+v", got)
	}
}

// Everything that stops the provider choosing a connection leaves every parsed
// attribute empty, so a provider configured from them fails to connect rather
// than reaching a cluster nobody chose. `raw` is still there for the caller.
func TestAConnectionThatCannotBeChosenIsLeftEmpty(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ body, says string }{
		"not yaml": {"clusters: [", "is not YAML"},
		"several contexts, none current": {strings.Replace(twoContexts, "current-context: training\n", "", 1),
			"names no current-context and has 2 contexts"},
		"no contexts": {"clusters: []\n", "has no contexts"},
		"current context missing": {strings.Replace(twoContexts, "current-context: training", "current-context: gone", 1),
			`current-context "gone" is not among its contexts`},
		"cluster missing": {strings.Replace(twoContexts, "{cluster: training, user", "{cluster: gone, user", 1),
			`context "training" names cluster "gone", which is not in the file`},
		"user missing": {strings.Replace(twoContexts, "user: training-user}", "user: gone}", 1),
			`context "training" names user "gone", which is not in the file`},
		"no server": {strings.Replace(twoContexts, "server: https://training.invalid:6443", "server: \"\"", 1),
			`cluster "training" has no server`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, warnings := parseKubeconfigConnection(tc.body)
			if got != (kubeconfigConnection{}) {
				t.Errorf("parsed attributes set: %+v", got)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], tc.says) {
				t.Errorf("warnings %q, want one saying %q", warnings, tc.says)
			}
		})
	}
}

// The API's contract says the kubeconfig is static and not wired to an exec
// plugin. If one arrives anyway, nothing usable can be handed over, and the
// whole connection stays null. A kubernetes or helm provider given a host and
// no credential falls back to KUBE_TOKEN or a local kubeconfig, and would send
// a local credential to the platform's cluster. With no host either, that
// can't happen, though the provider then fills `host` from KUBE_HOST and the
// like and can reach another cluster, which a postcondition on the ephemeral
// block stops.
func TestACredentialThisProviderCannotHandOverIsNamed(t *testing.T) {
	t.Parallel()
	base := `clusters:
- name: c
  cluster: {server: "https://c.invalid", certificate-authority-data: ` + b64(testCAPEM) + `}
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user:
`
	cases := map[string]struct{ user, says string }{
		"exec plugin":      {"    exec: {apiVersion: client.authentication.k8s.io/v1, command: kubelogin}\n", "an exec plugin"},
		"auth provider":    {"    auth-provider: {name: oidc}\n", `the "oidc" auth provider`},
		"token file":       {"    tokenFile: /var/run/token\n", "a token file"},
		"certificate file": {"    client-certificate: /tmp/c.pem\n    client-key: /tmp/k.pem\n", "a client certificate file"},
		"nothing":          {"    {}\n", "no credential"},
		"basic auth":       {"    {username: u, password: p}\n", "a username and password"},
		"half a cert pair": {"    client-certificate-data: " + b64(testCertPEM) + "\n", "half of a client certificate"},
		"cert not base64":  {"    {client-certificate-data: not!base64, client-key-data: not!base64}\n", "not base64"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, warnings := parseKubeconfigConnection(base + tc.user)
			if got != (kubeconfigConnection{}) {
				t.Errorf("parsed attributes set: %+v", got)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], tc.says) {
				t.Errorf("warnings %q, want one saying %q", warnings, tc.says)
			}
			// The warning says what to do instead, not only what's wrong.
			if !strings.Contains(warnings[0], "`raw`") {
				t.Errorf("warning doesn't point at raw: %q", warnings[0])
			}
		})
	}
}

// A CA the provider can't decode is not replaced by nothing silently: an empty
// cluster_ca_certificate makes the kubernetes provider trust the system roots.
func TestACAThatIsNotBase64IsNamed(t *testing.T) {
	t.Parallel()
	got, warnings := parseKubeconfigConnection(strings.Replace(twoContexts, b64(testCAPEM), "not!base64", 1))
	if got.ClusterCACertificate != "" || got.Host == "" || got.Token == "" {
		t.Errorf("got %+v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "certificate-authority-data is not base64") {
		t.Errorf("warnings %q", warnings)
	}
}

// A CA given as a file path names a file on whatever machine wrote the
// kubeconfig, not this one.
func TestACAFileIsNamed(t *testing.T) {
	t.Parallel()
	got, warnings := parseKubeconfigConnection(strings.Replace(twoContexts,
		"certificate-authority-data: "+b64(testCAPEM), "certificate-authority: /etc/ca.pem", 1))
	if got.ClusterCACertificate != "" || got.Host == "" {
		t.Errorf("got %+v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "a CA file") {
		t.Errorf("warnings %q", warnings)
	}
}

// A cluster setting the parsed attributes don't carry changes how a client
// connects, so dropping it is named, with the argument that takes it. A
// proxy URL can carry a password, so no value is quoted.
func TestAClusterSettingThatIsNotHandedOverIsNamed(t *testing.T) {
	t.Parallel()
	ca := "certificate-authority-data: " + b64(testCAPEM)
	for setting, c := range map[string]struct{ line, argument string }{
		"insecure-skip-tls-verify": {"insecure-skip-tls-verify: true", "`insecure`"},
		"tls-server-name":          {"tls-server-name: api.training.invalid", "`tls_server_name`"},
		"proxy-url":                {"proxy-url: http://ops:hunter2@proxy.invalid:3128", "`proxy_url`"},
	} {
		t.Run(setting, func(t *testing.T) {
			t.Parallel()
			got, warnings := parseKubeconfigConnection(strings.Replace(twoContexts, ca, ca+"\n    "+c.line, 1))
			if got.Host == "" || got.Token == "" || got.ClusterCACertificate == "" {
				t.Errorf("got %+v, want the connection handed over", got)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], setting) || !strings.Contains(warnings[0], c.argument) {
				t.Fatalf("warnings %q, want one naming %s and %s", warnings, setting, c.argument)
			}
			for _, leak := range []string{"hunter2", "api.training.invalid"} {
				if strings.Contains(warnings[0], leak) {
					t.Errorf("warning quotes %q: %q", leak, warnings[0])
				}
			}
		})
	}
	_, warnings := parseKubeconfigConnection(strings.Replace(twoContexts, ca, ca+"\n    insecure-skip-tls-verify: false", 1))
	if len(warnings) != 0 {
		t.Errorf("insecure-skip-tls-verify: false warns %q, want nothing: it's the default", warnings)
	}
}

// An exec plugin's command line can carry a secret of its own, so the warning
// names the mechanism and quotes none of it.
func TestAnExecPluginWarningQuotesNoCommand(t *testing.T) {
	t.Parallel()
	body := strings.Replace(twoContexts, "{token: training-token}",
		`{exec: {command: "/bin/get-token --password=hunter2", args: [--secret=s3cr3t]}}`, 1)
	_, warnings := parseKubeconfigConnection(body)
	if len(warnings) != 1 {
		t.Fatalf("warnings %q", warnings)
	}
	for _, leak := range []string{"hunter2", "get-token", "s3cr3t"} {
		if strings.Contains(warnings[0], leak) {
			t.Errorf("warning quotes %q: %q", leak, warnings[0])
		}
	}
}

// No warning text may quote a secret. A warning is printed to the console and
// the trace log, where `Sensitive` doesn't reach.
func TestAWarningNeverQuotesTheCredential(t *testing.T) {
	t.Parallel()
	body := strings.Replace(twoContexts, "current-context: training\n", "", 1)
	body = strings.Replace(body, "{token: training-token}", "{token: training-token, exec: {command: x}}", 1)
	_, warnings := parseKubeconfigConnection(body)
	for _, w := range warnings {
		if strings.Contains(w, "training-token") || strings.Contains(w, "other-token") {
			t.Errorf("warning quotes a token: %q", w)
		}
	}
}

// The parsed fields reach the ephemeral result, and the body is still there
// beside them.
func TestTheEphemeralResultCarriesTheConnection(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{clusterState: "active", body: twoContexts}
	e := &clusterKubeconfigEphemeral{meta: p.meta(t, time.Minute)}

	ctx := context.Background()
	var schemaResp ephemeral.SchemaResponse
	e.Schema(ctx, ephemeral.SchemaRequest{}, &schemaResp)
	object, ok := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatal("ephemeral schema is not an object")
	}
	values := make(map[string]tftypes.Value, len(object.AttributeTypes))
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	values["cluster_id"] = tftypes.NewValue(tftypes.String, "c1")
	config := tfsdk.Config{Raw: tftypes.NewValue(object, values), Schema: schemaResp.Schema}

	resp := ephemeral.OpenResponse{Result: tfsdk.EphemeralResultData{Schema: schemaResp.Schema}}
	e.Open(ctx, ephemeral.OpenRequest{Config: config}, &resp)
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() > 0 {
		t.Fatalf("diagnostics: %v", resp.Diagnostics)
	}
	var got clusterKubeconfigEphemeralModel
	resp.Diagnostics.Append(resp.Result.Get(ctx, &got)...)
	if resp.Diagnostics.HasError() {
		t.Fatalf("result: %v", resp.Diagnostics)
	}
	if got.Raw.ValueString() != twoContexts {
		t.Errorf("raw changed")
	}
	if got.Host.ValueString() != "https://training.invalid:6443" ||
		got.ClusterCACertificate.ValueString() != testCAPEM ||
		got.Token.ValueString() != "training-token" {
		t.Errorf("got host %q, ca %q, token set %t", got.Host.ValueString(),
			got.ClusterCACertificate.ValueString(), got.Token.ValueString() != "")
	}
	// What the kubeconfig doesn't carry is null, not an empty string, so the
	// kubernetes provider falls back to its own defaults for it.
	if !got.ClientCertificate.IsNull() || !got.ClientKey.IsNull() {
		t.Errorf("client certificate and key should be null for a token kubeconfig")
	}
}

// The credential-bearing outputs are marked, so Terraform withholds them from
// the console. The cluster's address and CA are not secret.
func TestTheSecretOutputsAreSensitive(t *testing.T) {
	t.Parallel()
	var resp ephemeral.SchemaResponse
	(&clusterKubeconfigEphemeral{}).Schema(context.Background(), ephemeral.SchemaRequest{}, &resp)
	want := map[string]bool{
		"raw": true, "token": true, "client_key": true,
		"host": false, "cluster_ca_certificate": false, "client_certificate": false,
	}
	for name, sensitive := range want {
		a, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Errorf("no attribute %s", name)
			continue
		}
		if a.IsSensitive() != sensitive {
			t.Errorf("%s: sensitive %t, want %t", name, a.IsSensitive(), sensitive)
		}
		if !a.IsComputed() || a.IsOptional() || a.IsRequired() {
			t.Errorf("%s should be computed only", name)
		}
	}
}
