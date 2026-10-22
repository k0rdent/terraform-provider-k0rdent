package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// providerSchema returns the real schema, so the config values below are
// decoded by the same path Terraform uses rather than a test-only imitation.
func providerSchema(t *testing.T) (*aiProvider, tftypes.Object) {
	t.Helper()

	p := &aiProvider{version: "test"}
	var resp fwprovider.SchemaResponse
	p.Schema(context.Background(), fwprovider.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Schema: %v", resp.Diagnostics.Errors())
	}

	obj, ok := resp.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("the provider schema is not an object type")
	}
	return p, obj
}

// configWith builds a provider block. Attributes absent from attrs are null,
// which is what Terraform sends for an attribute the user did not write.
func configWith(t *testing.T, obj tftypes.Object, attrs map[string]tftypes.Value) tfsdk.Config {
	t.Helper()

	values := make(map[string]tftypes.Value, len(obj.AttributeTypes))
	for name, typ := range obj.AttributeTypes {
		if v, ok := attrs[name]; ok {
			values[name] = v
			continue
		}
		values[name] = tftypes.NewValue(typ, nil)
	}

	p, _ := providerSchema(t)
	var resp fwprovider.SchemaResponse
	p.Schema(context.Background(), fwprovider.SchemaRequest{}, &resp)

	return tfsdk.Config{
		Raw:    tftypes.NewValue(obj, values),
		Schema: resp.Schema,
	}
}

func str(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }

// configure runs the real Configure against a config and an injected
// environment, and returns the Meta it produced.
func configure(t *testing.T, attrs map[string]tftypes.Value, env map[string]string) (*Meta, fwprovider.ConfigureResponse) {
	t.Helper()

	p, obj := providerSchema(t)
	p.lookupEnv = envMap(env)

	var resp fwprovider.ConfigureResponse
	p.Configure(context.Background(), fwprovider.ConfigureRequest{Config: configWith(t, obj, attrs)}, &resp)

	return metaFrom(t, resp), resp
}

// metaFrom returns the Meta Configure produced, or nil when it produced none.
func metaFrom(t *testing.T, resp fwprovider.ConfigureResponse) *Meta {
	t.Helper()

	if resp.ResourceData == nil {
		return nil
	}
	meta, ok := resp.ResourceData.(*Meta)
	if !ok {
		t.Fatalf("ResourceData is %T, want *Meta", resp.ResourceData)
	}
	return meta
}

func TestConfigureBuildsAClientAndShareItWithResourcesAndDataSources(t *testing.T) {
	t.Parallel()

	meta, resp := configure(t,
		map[string]tftypes.Value{
			"endpoint": str("https://api.dev02.example.com"),
			"region":   str("us-texas-1"),
			"project":  str("cp-production"),
		},
		saEnv(nil),
	)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure produced errors: %v", resp.Diagnostics.Errors())
	}
	if meta == nil {
		t.Fatal("Configure set no ResourceData")
	}
	if meta.Client == nil {
		t.Fatal("Meta carries no client")
	}
	if resp.ResourceData != resp.DataSourceData {
		t.Error("resources and data sources were given different values; they must share one client")
	}
	if meta.Endpoint != "https://api.dev02.example.com" {
		t.Errorf("Endpoint = %q", meta.Endpoint)
	}
	if meta.Region != "us-texas-1" || meta.Project != "cp-production" {
		t.Errorf("targeting = region %q project %q", meta.Region, meta.Project)
	}
}

func TestConfigureReadsTheEnvironmentWhenTheBlockIsEmpty(t *testing.T) {
	t.Parallel()

	meta, resp := configure(t, nil, saEnv(map[string]string{
		EnvVar(envEndpoint): "https://api.dev01.example.com",
		EnvVar(envRegion):   "eu-west-1",
		EnvVar(envProject):  "cp-example",
	}))

	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure produced errors: %v", resp.Diagnostics.Errors())
	}
	if meta.Endpoint != "https://api.dev01.example.com" || meta.Region != "eu-west-1" || meta.Project != "cp-example" {
		t.Fatalf("got endpoint=%q region=%q project=%q", meta.Endpoint, meta.Region, meta.Project)
	}
}

// Terraform sends an object with null attributes for an absent `provider`
// block, not a null object. Decoding a null object into a struct produces the
// framework's "this is always an error in the provider, please report it"
// diagnostic, which is the worst possible way to learn you have no credential.
func TestConfigureHandlesANullConfigurationObject(t *testing.T) {
	t.Parallel()

	p, obj := providerSchema(t)
	p.lookupEnv = envMap(saEnv(map[string]string{EnvVar(envRegion): "us-texas-1"}))

	var schemaResp fwprovider.SchemaResponse
	p.Schema(context.Background(), fwprovider.SchemaRequest{}, &schemaResp)

	var resp fwprovider.ConfigureResponse
	p.Configure(context.Background(), fwprovider.ConfigureRequest{
		Config: tfsdk.Config{Raw: tftypes.NewValue(obj, nil), Schema: schemaResp.Schema},
	}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("a null configuration object produced errors: %v", resp.Diagnostics.Errors())
	}
	meta := metaFrom(t, resp)
	if meta == nil || meta.Region != "us-texas-1" || meta.Endpoint != DefaultEndpoint {
		t.Fatalf("the environment did not supply the configuration: %+v", meta)
	}
}

func TestConfigureFailsWithoutACredential(t *testing.T) {
	t.Parallel()

	meta, resp := configure(t, nil, nil)

	if !resp.Diagnostics.HasError() {
		t.Fatal("Configure succeeded with no credential in the environment")
	}
	if meta != nil {
		t.Error("Configure set ResourceData despite failing")
	}
	if !containsSummary(resp.Diagnostics.Errors(), "No credential") {
		t.Fatalf("wrong error: %v", resp.Diagnostics.Errors())
	}
}

// An attribute that is not known until apply cannot configure the provider at
// plan time. Terraform's generic message for that sends people looking in the
// wrong place, so the provider names the attribute and the way around it.
func TestConfigureRejectsUnknownAttributesByName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"endpoint", "region", "project"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, resp := configure(t,
				map[string]tftypes.Value{name: tftypes.NewValue(tftypes.String, tftypes.UnknownValue)},
				saEnv(nil),
			)

			if !resp.Diagnostics.HasError() {
				t.Fatalf("an unknown %q was accepted", name)
			}
			detail := resp.Diagnostics.Errors()[0].Detail()
			if !strings.Contains(detail, name) {
				t.Errorf("the error does not name the attribute:\n%s", detail)
			}
		})
	}
}

func TestConfigureSurfacesValidationErrors(t *testing.T) {
	t.Parallel()

	_, resp := configure(t, map[string]tftypes.Value{"region": str("US-WEST")}, saEnv(nil))

	if !containsSummary(resp.Diagnostics.Errors(), "Invalid region") {
		t.Fatalf("wrong diagnostics: %v", resp.Diagnostics.Errors())
	}
}

// ---------------------------------------------------------------------------
// End to end against a fake control plane
// ---------------------------------------------------------------------------

// fakeAPI is the smallest control plane that can prove the wiring: the
// anonymous token endpoint on the global tier, and one regional read that
// requires the minted token. It is not a stand-in for a real landscape, but
// everything between the
// provider block and the Authorization header is real.
type fakeAPI struct {
	*httptest.Server

	mints         atomic.Int64
	lastGrant     atomic.Value // map[string]any
	lastAuth      atomic.Value // string
	lastUserAgent atomic.Value // string
	mintUserAgent atomic.Value // string
	lastPath      atomic.Value // string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()

	f := &fakeAPI{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/regions/global/auth/token", func(w http.ResponseWriter, r *http.Request) {
		f.mints.Add(1)
		f.mintUserAgent.Store(r.Header.Get("User-Agent"))

		var grant map[string]any
		if err := json.NewDecoder(r.Body).Decode(&grant); err != nil {
			http.Error(w, `{"error":{"code":400,"message":"undecodable grant"}}`, http.StatusBadRequest)
			return
		}
		f.lastGrant.Store(grant)

		// The mint is anonymous; a bearer header here would mean the client
		// tried to authenticate its own token request.
		if r.Header.Get("Authorization") != "" {
			http.Error(w, `{"error":{"code":400,"message":"the mint must be anonymous"}}`, http.StatusBadRequest)
			return
		}

		writeJSON(w, map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   900,
		})
	})

	mux.HandleFunc("GET /v1/regions/{region}/compute/cluster-types", func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth.Store(r.Header.Get("Authorization"))
		f.lastUserAgent.Store(r.Header.Get("User-Agent"))
		f.lastPath.Store(r.URL.Path)

		writeJSON(w, map[string]any{"clusterTypes": []any{}})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func writeJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (f *fakeAPI) stringValue(t *testing.T, v *atomic.Value) string {
	t.Helper()

	s, ok := v.Load().(string)
	if !ok {
		t.Fatal("the fake recorded no value; the request never arrived")
	}
	return s
}

// grant returns the body of the most recent mint.
func (f *fakeAPI) grant(t *testing.T) map[string]any {
	t.Helper()

	g, ok := f.lastGrant.Load().(map[string]any)
	if !ok {
		t.Fatal("the fake recorded no mint; the client never asked for a token")
	}
	return g
}

// The goal is a `terraform plan` that authenticates. Against the fake, this
// is the closest honest substitute: a real provider block, resolved by the real Configure, building
// the real generated client, which mints a real token over HTTP and presents
// it on a real regional read. Only the control plane is fake.
func TestConfiguredProviderAuthenticatesAgainstAFakeControlPlane(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)

	meta, resp := configure(t,
		map[string]tftypes.Value{"endpoint": str(api.URL), "region": str("us-texas-1")},
		saEnv(nil),
	)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure: %v", resp.Diagnostics.Errors())
	}

	// Configure makes no call of its own: nothing is spent before a resource
	// asks for something.
	if got := api.mints.Load(); got != 0 {
		t.Fatalf("Configure minted %d tokens; it must make no API call", got)
	}

	region, diags := meta.RequireRegion()
	if diags.HasError() {
		t.Fatalf("RequireRegion: %v", diags.Errors())
	}

	r, err := meta.Client.ListClusterTypesWithResponse(context.Background(), region, nil)
	if err != nil {
		t.Fatalf("ListClusterTypes: %v", err)
	}
	if r.StatusCode() != http.StatusOK {
		t.Fatalf("ListClusterTypes returned %s: %s", r.Status(), r.Body)
	}

	if got, want := api.stringValue(t, &api.lastAuth), "Bearer fake-access-token"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got, want := api.stringValue(t, &api.lastPath), "/v1/regions/us-texas-1/compute/cluster-types"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if got, want := api.stringValue(t, &api.lastUserAgent), UserAgent("test", "", ""); got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}

	grant := api.grant(t)
	if grant["grant_type"] != "client_credentials" {
		t.Errorf("grant_type = %v, want client_credentials", grant["grant_type"])
	}
	if grant["client_id"] != "sa-terraform@vendor.iam" || grant["client_secret"] != "k0r_secret" {
		t.Errorf("the mint did not carry the environment's credential: %v", grant)
	}
	// Scope narrowing is deliberately not requested. The contract offers it on
	// the machine grants and the platform discards it unread, so sending it
	// would put a claim in the request that nothing honours. See
	// CredentialSource.build.
	if _, ok := grant["scope_narrowing"]; ok {
		t.Errorf("the mint requested scope narrowing: %v", grant)
	}
}

func TestAPIKeyCredentialMintsWithTheAPIKeyGrant(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)

	meta, resp := configure(t,
		map[string]tftypes.Value{"endpoint": str(api.URL)},
		map[string]string{EnvVar(envAPIKey): "k0r_apikey"},
	)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure: %v", resp.Diagnostics.Errors())
	}
	if resp.Diagnostics.WarningsCount() == 0 {
		t.Error("configuring with an API key produced no warning")
	}

	if _, err := meta.Client.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err != nil {
		t.Fatalf("ListClusterTypes: %v", err)
	}

	grant := api.grant(t)
	if grant["grant_type"] != "api_key" || grant["api_key"] != "k0r_apikey" {
		t.Fatalf("unexpected grant: %v", grant)
	}
	if _, ok := grant["scope_narrowing"]; ok {
		t.Errorf("the api_key mint requested scope narrowing: %v", grant)
	}
}

// ---------------------------------------------------------------------------
// The real environment
// ---------------------------------------------------------------------------

// Every other test injects the environment, which would keep passing if
// Configure stopped reading the real one. This one does not, so it cannot run
// in parallel.
func TestConfigureReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv(EnvVar(envClientID), "sa-terraform@vendor.iam")
	t.Setenv(EnvVar(envClientSecret), "k0r_secret")
	t.Setenv(EnvVar(envRegion), "us-texas-1")

	p, ok := New("test")().(*aiProvider)
	if !ok {
		t.Fatal("New did not return the provider implementation")
	}
	if p.lookupEnv != nil {
		t.Fatal("New installed a test environment hook")
	}

	_, obj := providerSchema(t)
	var resp fwprovider.ConfigureResponse
	p.Configure(context.Background(), fwprovider.ConfigureRequest{Config: configWith(t, obj, nil)}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure: %v", resp.Diagnostics.Errors())
	}
	meta := metaFrom(t, resp)
	if meta == nil || meta.Region != "us-texas-1" {
		t.Fatalf("Configure did not read the process environment: %+v", meta)
	}
	if meta.Endpoint != DefaultEndpoint {
		t.Fatalf("Endpoint = %q, want the default %q", meta.Endpoint, DefaultEndpoint)
	}
}

// HashiCorp's convention puts the Terraform that ran the request first, then
// the provider. Both sides of a version skew report are then in the gateway
// log for every request, the mint included, without asking whoever ran it.
func TestEveryRequestNamesTerraformAndTheProvider(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	p, obj := providerSchema(t)
	p.lookupEnv = envMap(saEnv(nil))
	var resp fwprovider.ConfigureResponse
	p.Configure(context.Background(), fwprovider.ConfigureRequest{
		Config: configWith(t, obj, map[string]tftypes.Value{
			"endpoint": str(api.URL), "region": str("us-texas-1"),
		}),
		TerraformVersion: "1.9.5",
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure: %v", resp.Diagnostics.Errors())
	}

	r, err := metaFrom(t, resp).Client.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
	if err != nil || r.StatusCode() != http.StatusOK {
		t.Fatalf("ListClusterTypes: %v %v", err, r)
	}

	want := "Terraform/1.9.5 (+https://www.terraform.io) terraform-provider-" + ProviderTypeName + "/test"
	if got := api.stringValue(t, &api.lastUserAgent); got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
	if got := api.stringValue(t, &api.mintUserAgent); got != want {
		t.Errorf("the mint's User-Agent = %q, want %q", got, want)
	}
}

// TF_APPEND_USER_AGENT is how a pipeline or a wrapper names itself on every
// provider's requests. HashiCorp's SDK appends it, trimmed, after its own
// product tokens, and so do the large providers, so a gateway log can tell
// an apply from CI from one run by hand. A value of only whitespace adds
// nothing.
func TestEveryRequestCarriesTheAppendedUserAgent(t *testing.T) {
	t.Parallel()

	base := "Terraform/1.9.5 (+https://www.terraform.io) terraform-provider-" + ProviderTypeName + "/test"
	for appended, want := range map[string]string{
		"  release-pipeline/2.0 (+https://ci.example) \n": base + " release-pipeline/2.0 (+https://ci.example)",
		" \t ": base,
	} {
		api := newFakeAPI(t)
		p, obj := providerSchema(t)
		env := saEnv(map[string]string{"TF_APPEND_USER_AGENT": appended})
		p.lookupEnv = envMap(env)
		var resp fwprovider.ConfigureResponse
		p.Configure(context.Background(), fwprovider.ConfigureRequest{
			Config: configWith(t, obj, map[string]tftypes.Value{
				"endpoint": str(api.URL), "region": str("us-texas-1"),
			}),
			TerraformVersion: "1.9.5",
		}, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("Configure: %v", resp.Diagnostics.Errors())
		}
		r, err := metaFrom(t, resp).Client.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
		if err != nil || r.StatusCode() != http.StatusOK {
			t.Fatalf("ListClusterTypes: %v %v", err, r)
		}
		if got := api.stringValue(t, &api.lastUserAgent); got != want {
			t.Errorf("TF_APPEND_USER_AGENT %q: User-Agent = %q, want %q", appended, got, want)
		}
		if got := api.stringValue(t, &api.mintUserAgent); got != want {
			t.Errorf("TF_APPEND_USER_AGENT %q: the mint's User-Agent = %q, want %q", appended, got, want)
		}
	}
}
