package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	peerLocal  = "/v1/regions/test/projects/test/networking/vpcs/vpc-a"
	peerRemote = "/v1/regions/test/projects/test/networking/vpcs/vpc-b"
)

// peeringServer answers a peering create on vpc-a pointing at vpc-b, the
// reads of it, and the list of vpc-b's own peerings with status and back. A
// status other than 200 answers with refusal as the error's message.
func peeringServer(t *testing.T, status int, back []map[string]any, refusal string) *httptest.Server {
	t.Helper()
	ours := map[string]any{
		"id": "p1", "state": "active", "localVpc": peerLocal, "remoteVpc": peerRemote,
		"selfLink": peerLocal + "/peerings/p1",
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch {
		case r.URL.Path == peerLocal+"/peerings" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			body = ours
		case r.URL.Path == peerLocal+"/peerings/p1" && r.Method == http.MethodGet:
			body = ours
		case r.URL.Path == peerRemote+"/peerings" && r.Method == http.MethodGet:
			w.WriteHeader(status)
			if status != http.StatusOK {
				body = map[string]any{"error": map[string]any{"code": status, "message": refusal}}
				break
			}
			body = map[string]any{"peerings": back}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			body = map[string]any{"error": map[string]any{"code": 404, "message": "not found"}}
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
}

// createPeeringToward runs Create for p1 on vpc-a toward vpc-b and returns its
// diagnostics, failing the test on any error.
func createPeeringToward(t *testing.T, server *httptest.Server) diag.Diagnostics {
	t.Helper()
	return createPeeringTo(t, server, peerRemote)
}

// createPeeringTo is createPeeringToward with the remote VPC named.
func createPeeringTo(t *testing.T, server *httptest.Server, remote string) diag.Diagnostics {
	t.Helper()
	r := &vpcPeeringResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	planRaw, s := testValue(t, r, map[string]tftypes.Value{
		"id": str("p1"), "vpc": str("vpc-a"), "remote_vpc": str(remote),
		"region": str("test"), "project": str("test"),
	})
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create failed: %v", resp.Diagnostics.Errors())
	}
	return resp.Diagnostics
}

func warningText(diags diag.Diagnostics) string {
	var b strings.Builder
	for _, w := range diags.Warnings() {
		b.WriteString(w.Summary() + "\n" + w.Detail() + "\n")
	}
	return b.String()
}

// Live on dev.4, both VPCs in one project: the remote VPC listed the mirrored
// peering, and the create still warned that it wasn't listed by the API and
// had to be confirmed out of band. When the far side is readable and points
// back, there's nothing to warn about.
func TestVPCPeeringCreateSaysNothingWhenTheMirrorIsThere(t *testing.T) {
	server := peeringServer(t, http.StatusOK, []map[string]any{{
		"id": "p1-back", "state": "active", "localVpc": peerRemote, "remoteVpc": peerLocal,
	}}, "")
	defer server.Close()
	if got := warningText(createPeeringToward(t, server)); got != "" {
		t.Fatalf("warned with the mirrored peering active on the far side:\n%s", got)
	}
}

// Readable and with nothing pointing back, the warning states it as a fact,
// naming the remote VPC that lists no peering, rather than as a condition to
// check elsewhere.
func TestVPCPeeringCreateSaysTheFarSideListsNoMirror(t *testing.T) {
	server := peeringServer(t, http.StatusOK, []map[string]any{{
		"id": "elsewhere", "state": "active", "localVpc": peerRemote,
		"remoteVpc": "/v1/regions/test/projects/test/networking/vpcs/vpc-c",
	}}, "")
	defer server.Close()
	got := warningText(createPeeringToward(t, server))
	if !strings.Contains(got, `"`+peerRemote+`" lists no peering pointing back`) {
		t.Fatalf("the warning doesn't say the far side lists no mirror:\n%s", got)
	}
	if strings.Contains(got, "not listed by this API") || strings.Contains(got, "out of band") {
		t.Fatalf("the warning sends the reader out of band for what it just read:\n%s", got)
	}
}

// A mirror that's there but not active yet is named with its state.
func TestVPCPeeringCreateNamesAMirrorThatIsNotActive(t *testing.T) {
	server := peeringServer(t, http.StatusOK, []map[string]any{{
		"id": "p1-back", "state": "failed", "localVpc": peerRemote, "remoteVpc": peerLocal,
	}}, "")
	defer server.Close()
	got := warningText(createPeeringToward(t, server))
	if !strings.Contains(got, `"p1-back"`) || !strings.Contains(got, "`failed`") {
		t.Fatalf("the warning doesn't name the mirror and its state:\n%s", got)
	}
}

// Unreadable, which is the usual case across projects, the warning keeps the
// condition and says what the platform answered when asked.
func TestVPCPeeringCreateSaysWhyTheFarSideWasNotChecked(t *testing.T) {
	server := peeringServer(t, http.StatusForbidden, nil, "forbidden")
	defer server.Close()
	got := warningText(createPeeringToward(t, server))
	if !strings.Contains(got, "403") || !strings.Contains(got, "out of band") {
		t.Fatalf("the warning doesn't say the far side couldn't be read, and why:\n%s", got)
	}
	if strings.Contains(got, "not listed by this API") {
		t.Fatalf("the warning blames the API for a refusal:\n%s", got)
	}
}

// hostilePeerText is what a platform answer must not carry into a terminal as is: an
// ESC that starts a terminal sequence, a bidi override that reorders what's
// shown, and a bearer token.
const hostilePeerText = "forbidden \x1b[2J\u202e Bearer abc.def"

func assertDefused(t *testing.T, got string) {
	t.Helper()
	if strings.ContainsAny(got, "\x1b\u202e") {
		t.Errorf("a control or bidi character reached the warning raw:\n%q", got)
	}
	if strings.Contains(got, "abc.def") {
		t.Errorf("a bearer token reached the warning:\n%q", got)
	}
}

// The platform's refusal is quoted in the fallback warning, so it goes
// through the same redaction and escaping as every other diagnostic.
func TestVPCPeeringCreateDefusesTheRefusalItQuotes(t *testing.T) {
	server := peeringServer(t, http.StatusForbidden, nil, hostilePeerText)
	defer server.Close()
	got := warningText(createPeeringToward(t, server))
	if !strings.Contains(got, "403") {
		t.Fatalf("the warning doesn't quote the refusal at all:\n%s", got)
	}
	assertDefused(t, got)
}

// A mirror's id and state are the platform's text too.
func TestVPCPeeringCreateDefusesTheMirrorItNames(t *testing.T) {
	server := peeringServer(t, http.StatusOK, []map[string]any{{
		"id": "p1-back " + hostilePeerText, "state": "failed " + hostilePeerText, "localVpc": peerRemote, "remoteVpc": peerLocal,
	}}, "")
	defer server.Close()
	got := warningText(createPeeringToward(t, server))
	if !strings.Contains(got, "p1-back") {
		t.Fatalf("the warning doesn't name the mirror:\n%s", got)
	}
	assertDefused(t, got)
}

// A mirror being deleted and a new one already active both point back. The
// active one is what carries traffic, whichever the list puts first.
func TestVPCPeeringCreatePrefersAnActiveMirror(t *testing.T) {
	server := peeringServer(t, http.StatusOK, []map[string]any{
		{"id": "old-back", "state": "deleting", "localVpc": peerRemote, "remoteVpc": peerLocal},
		{"id": "new-back", "state": "active", "localVpc": peerRemote, "remoteVpc": peerLocal},
	}, "")
	defer server.Close()
	if got := warningText(createPeeringToward(t, server)); got != "" {
		t.Fatalf("warned about a deleting mirror with an active one beside it:\n%s", got)
	}
}

// The provider reads only the region and project this peering is in. A remote
// VPC anywhere else isn't read at all, so the create sends no request outside
// that scope, and the warning says the far side wasn't checked and why, rather
// than reporting a refusal it never asked for.
func TestVPCPeeringCreateDoesNotReadOutsideItsScope(t *testing.T) {
	for _, remote := range []string{
		"/v1/regions/test/projects/other/networking/vpcs/vpc-elsewhere",
		"/v1/regions/far/projects/test/networking/vpcs/vpc-elsewhere",
	} {
		t.Run(remote, func(t *testing.T) {
			// The server fails the test on any request to the remote VPC.
			server := peeringServer(t, http.StatusOK, nil, "")
			defer server.Close()
			got := warningText(createPeeringTo(t, server, remote))
			if !strings.Contains(got, "out of band") || !strings.Contains(got, "outside") {
				t.Fatalf("the warning doesn't say the far side is outside what's read:\n%s", got)
			}
			if strings.Contains(got, "lists no peering") {
				t.Fatalf("the warning reports a far side it never read:\n%s", got)
			}
		})
	}
}
