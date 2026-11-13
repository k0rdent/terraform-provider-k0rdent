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
	"time"

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
// warned of at plan and refused at apply, and a move refused at plan.

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

// A project the list doesn't place still gets a diagnostic that says what to
// do and where to read the region. That isn't the project list again, which
// the lookup already read: it's the read that names the organization, which
// tells two organizations' projects apart, and which falls back to the list
// for a credential scoped to the project.
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
	for _, want := range []string{"another region", "nor the project list told the provider which",
		ResourceName(projectDataSourceSuffix) + "` data source with `organization_id` set", "`region`"} {
		if !strings.Contains(detail, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, detail)
		}
	}
}

// planNetworkCreate runs modifyScopePlan for a network created in region,
// or moved there from from when from isn't empty.
func planNetworkCreate(t *testing.T, meta *Meta, region string) resource.ModifyPlanResponse {
	t.Helper()
	return planNetworkIn(t, meta, "", region)
}

func planNetworkIn(t *testing.T, meta *Meta, from, region string) resource.ModifyPlanResponse {
	t.Helper()
	r := &networkResource{meta: meta}
	set := map[string]tftypes.Value{"id": str("n"), "project": str("p")}
	if region != "" {
		set["region"] = str(region)
	}
	configRaw, s := testValue(t, r, set)
	plan := tfsdk.Plan{Raw: configRaw, Schema: s.Schema}
	state := tfsdk.State{Raw: tftypes.NewValue(configRaw.Type(), nil), Schema: s.Schema}
	if from != "" {
		prior := map[string]tftypes.Value{"id": str("n"), "project": str("p"), "region": str(from)}
		stateRaw, _ := testValue(t, r, prior)
		state.Raw = stateRaw
	}
	resp := resource.ModifyPlanResponse{Plan: plan}
	modifyScopePlan(context.Background(), meta, resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
		Plan:   plan,
		State:  state,
	}, &resp)
	return resp
}

// A create whose project is known to be in another region only warns at
// plan, whether its region came from the resource or the provider block: a
// create deletes nothing, so the apply refuses it, and `terraform destroy`,
// which plans the create of a resource state doesn't have, isn't stopped.
func TestACreateInAnotherRegionThanItsProjectWarnsAtPlan(t *testing.T) {
	api := &mismatchAPI{home: "us-east"}
	meta := mismatchMeta(t, api)
	for _, region := range []string{"eu", ""} {
		resp := planNetworkCreate(t, meta, region)
		if resp.Diagnostics.HasError() {
			t.Fatalf("region %q: refused at plan: %v", region, resp.Diagnostics)
		}
		var warning diag.Diagnostic
		for _, w := range resp.Diagnostics.Warnings() {
			if w.Summary() == regionRefusal {
				warning = w
			}
		}
		if warning == nil {
			t.Fatalf("region %q: no warning: %v", region, resp.Diagnostics)
		}
		for _, want := range []string{`"p"`, "us-east", "eu", checkedAtCreate} {
			if !strings.Contains(warning.Detail(), want) {
				t.Errorf("region %q: warning does not say %q:\n%s", region, want, warning.Detail())
			}
		}
	}
	if resp := planNetworkCreate(t, meta, "us-east"); len(resp.Diagnostics) > 0 {
		t.Errorf("the project's own region warned: %v", resp.Diagnostics)
	}

	// A project the list doesn't show, being created in this apply or out of
	// the credential's sight, is left to the platform.
	api.hidden = true
	if resp := planNetworkCreate(t, mismatchMeta(t, api), "eu"); len(resp.Diagnostics) > 0 {
		t.Errorf("an unseen project warned: %v", resp.Diagnostics)
	}
}

// A move to a region the project isn't in is a replacement, whose delete
// would come before the create's refusal, so it's refused at plan.
func TestAMoveToAnotherRegionThanItsProjectIsRefusedAtPlan(t *testing.T) {
	meta := mismatchMeta(t, &mismatchAPI{home: "us-east"})
	resp := planNetworkIn(t, meta, "us-east", "eu")
	if !resp.Diagnostics.HasError() {
		t.Fatalf("the move planned: %v", resp.Diagnostics)
	}
	e := resp.Diagnostics.Errors()[0]
	for _, want := range []string{`"p"`, "us-east", replacedOnThisPlan} {
		if !strings.Contains(e.Detail(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, e.Detail())
		}
	}
	if e.Summary() != regionRefusal {
		t.Errorf("summary = %q", e.Summary())
	}
}

// Every kind's create refuses a region its project isn't in before it sends
// anything, since the plan only warned. The project list is the one request.
func TestACreateInAnotherRegionThanItsProjectIsRefusedBeforeAnyRequest(t *testing.T) {
	for kind, k := range pendingKinds() {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			var sent []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				sent = append(sent, r.Method+" "+r.URL.Path)
				mu.Unlock()
				if r.URL.Path == "/v1/regions/global/projects" {
					writeJSON(w, map[string]any{"projects": []any{
						map[string]any{"id": "elsewhere", "organizationId": "o", "region": "home"},
					}, "totalCount": 1})
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)
			r := k.build(testClusterMeta(t, server.URL, time.Minute))
			planRaw, s := testValue(t, r, k.plan(t, r))
			resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
			r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
			if !resp.Diagnostics.HasError() {
				t.Fatal("the create wasn't refused")
			}
			e := resp.Diagnostics.Errors()[0]
			if e.Summary() != regionRefusal {
				t.Errorf("summary = %q: %s", e.Summary(), e.Detail())
			}
			for _, want := range []string{`"elsewhere"`, `"home"`, `"test"`, createSentNothing} {
				if !strings.Contains(e.Detail(), want) {
					t.Errorf("the refusal does not say %q:\n%s", want, e.Detail())
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if got := strings.Join(sent, ", "); got != "GET /v1/regions/global/projects" {
				t.Errorf("sent %s, want only the project list", got)
			}
			if !resp.State.Raw.IsNull() {
				t.Error("the refused create wrote state")
			}
		})
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
