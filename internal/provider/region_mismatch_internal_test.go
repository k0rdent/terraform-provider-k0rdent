package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// v1.3.0-dev.4 answers every route under /v1/regions/{region}/projects/{project}
// with a 409 PROJECT_REGION_MISMATCH when the project is locked to another
// region, and the message doesn't say which. These pin that the provider
// says which, and that a create it can tell is in the wrong region is
// refused at plan.

// mismatchAPI holds project p in region home. Every project-scoped route in
// another region answers the contract's 409, and the project list answers
// with p unless hidden is set.
type mismatchAPI struct {
	mu     sync.Mutex
	home   string
	hidden bool
	lists  int
}

func (a *mismatchAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/regions/global/auth/token":
			writeJSON(w, map[string]any{"access_token": "t", "token_type": "Bearer", "expires_in": 3600})
		case r.URL.Path == "/v1/regions/global/projects":
			a.lists++
			rows := []any{}
			if !a.hidden {
				rows = append(rows, map[string]any{"id": "p", "organizationId": "o", "region": a.home})
			}
			writeJSON(w, map[string]any{"projects": rows, "totalCount": len(rows)})
		case strings.HasPrefix(r.URL.Path, "/v1/regions/"+a.home+"/projects/p/"):
			writeJSON(w, map[string]any{"id": "n", "state": "active"})
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			if _, err := w.Write([]byte(`{"error":{"code":409,"message":"project \"p\" is not in region \"eu\"",` +
				`"reason":"PROJECT_REGION_MISMATCH","requestId":"req-mismatch"}}`)); err != nil {
				t.Errorf("write: %v", err)
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func mismatchMeta(t *testing.T, api *mismatchAPI) *Meta {
	t.Helper()
	meta, err := newMeta(Settings{Endpoint: api.serve(t).URL, Region: "eu", Project: "p",
		Credential: CredentialSource{Kind: APIKeyCredential, APIKey: "k"}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestARegionMismatchNamesTheProjectsRegion(t *testing.T) {
	api := &mismatchAPI{home: "us-east"}
	meta := mismatchMeta(t, api)
	got, err := meta.Client.GetNetworkWithResponse(context.Background(), "eu", "p", "n")
	if err != nil {
		t.Fatal(err)
	}
	var diags diag.Diagnostics
	addAPIError(&diags, "read the network", got.HTTPResponse, got.Body, nil)
	if !diags.HasError() {
		t.Fatal("a 409 reported no error")
	}
	detail := diags.Errors()[0].Detail()
	for _, want := range []string{"PROJECT_REGION_MISMATCH", `"p"`, "us-east", "`region`", "req-mismatch"} {
		if !strings.Contains(detail, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, detail)
		}
	}
}

// The project list is read once per project, however many resources meet
// the refusal.
func TestARegionMismatchReadsTheProjectOnce(t *testing.T) {
	api := &mismatchAPI{home: "us-east"}
	meta := mismatchMeta(t, api)
	for range 3 {
		if _, err := meta.Client.GetNetworkWithResponse(context.Background(), "eu", "p", "n"); err != nil {
			t.Fatal(err)
		}
	}
	if api.lists != 1 {
		t.Errorf("the project list was read %d times, want 1", api.lists)
	}
}

// A project the credential can't see still gets a diagnostic that says what
// to do and where to read the region.
func TestARegionMismatchWithAnUnseenProjectSaysWhereToLook(t *testing.T) {
	api := &mismatchAPI{home: "us-east", hidden: true}
	meta := mismatchMeta(t, api)
	got, err := meta.Client.GetNetworkWithResponse(context.Background(), "eu", "p", "n")
	if err != nil {
		t.Fatal(err)
	}
	var diags diag.Diagnostics
	addAPIError(&diags, "read the network", got.HTTPResponse, got.Body, nil)
	detail := diags.Errors()[0].Detail()
	for _, want := range []string{"another region", ResourceName(projectDataSourceSuffix), "`region`"} {
		if !strings.Contains(detail, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, detail)
		}
	}
}

// planNetworkCreate runs modifyScopePlan for a network created in region.
func planNetworkCreate(t *testing.T, meta *Meta, region string) resource.ModifyPlanResponse {
	t.Helper()
	r := &networkResource{meta: meta}
	set := map[string]tftypes.Value{"id": str("n"), "project": str("p")}
	if region != "" {
		set["region"] = str(region)
	}
	configRaw, s := testValue(t, r, set)
	plan := tfsdk.Plan{Raw: configRaw, Schema: s.Schema}
	resp := resource.ModifyPlanResponse{Plan: plan}
	modifyScopePlan(context.Background(), meta, resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
		Plan:   plan,
		State:  tfsdk.State{Raw: tftypes.NewValue(configRaw.Type(), nil), Schema: s.Schema},
	}, &resp)
	return resp
}

// A create whose project is known to be in another region is refused at plan,
// whether its region came from the resource or the provider block.
func TestACreateInAnotherRegionThanItsProjectIsRefusedAtPlan(t *testing.T) {
	api := &mismatchAPI{home: "us-east"}
	meta := mismatchMeta(t, api)
	for _, region := range []string{"eu", ""} {
		resp := planNetworkCreate(t, meta, region)
		if !resp.Diagnostics.HasError() {
			t.Fatalf("region %q: planned", region)
		}
		detail := resp.Diagnostics.Errors()[0].Detail()
		for _, want := range []string{`"p"`, "us-east", "eu"} {
			if !strings.Contains(detail, want) {
				t.Errorf("region %q: diagnostic does not say %q:\n%s", region, want, detail)
			}
		}
	}
	if resp := planNetworkCreate(t, meta, "us-east"); resp.Diagnostics.HasError() {
		t.Errorf("the project's own region refused: %v", resp.Diagnostics)
	}

	// A project the list doesn't show, being created in this apply or out of
	// the credential's sight, is left to the platform.
	api.hidden = true
	if resp := planNetworkCreate(t, mismatchMeta(t, api), "eu"); resp.Diagnostics.HasError() {
		t.Errorf("an unseen project refused: %v", resp.Diagnostics)
	}
}

// A refusal whose body can't be read is handed back as it came: the caller's
// read fails with the same error, and no lookup is made.
func TestARefusalWhoseBodyFailsIsPassedOn(t *testing.T) {
	looked := false
	tr := &regionMismatchTransport{lookup: func(context.Context, string) (string, bool) {
		looked = true
		return "", false
	}}
	broken := errors.New("connection reset")
	req := sentRequest(t, func(c *k0rdent.Client) (*http.Response, error) {
		return c.GetNetwork(context.Background(), "eu", "p", "n")
	})
	resp := tr.annotate(req, &http.Response{StatusCode: http.StatusConflict, Header: http.Header{},
		Body: io.NopCloser(io.MultiReader(strings.NewReader(`{"error":`), failingReader{broken}))})
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, broken) {
		t.Errorf("read = %v, want %v", err, broken)
	}
	if looked || resp.Header.Get(mismatchProjectHeader) != "" {
		t.Error("a body that couldn't be read was taken for a region mismatch")
	}
}

// sentRequest is the request call makes through the generated client, caught
// before it's sent, so it carries its operation id as the provider's do.
func sentRequest(t *testing.T, call func(*k0rdent.Client) (*http.Response, error)) *http.Request {
	t.Helper()
	notSent := errors.New("not sent")
	var sent *http.Request
	c, err := k0rdent.NewClient("http://x", k0rdent.WithHTTPClient(doerFunc(func(req *http.Request) (*http.Response, error) {
		sent = req
		return nil, notSent
	})))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(c); !errors.Is(err, notSent) || sent == nil {
		t.Fatalf("the call sent no request: %v", err)
	}
	return sent
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }
