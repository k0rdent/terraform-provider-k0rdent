package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A regional resource in a different region from its project.
//
// The k0rdent API v1.3.0-dev.4 locks a project to one region and
// checks every compute, networking and storage route under
// /v1/regions/{region}/projects/{project} against it. A request naming
// another region gets a 409 with reason PROJECT_REGION_MISMATCH, whose message
// doesn't name the project's region. The way out is always the same, set
// `region` to the project's, so the provider looks the region up and says it:
// at plan time for a create whose project it can already read, and in the
// diagnostic of any request the platform refuses this way.

// regionMismatch is the reason the platform gives.
const regionMismatch = "PROJECT_REGION_MISMATCH"

// Headers regionMismatchTransport adds to a refused response, for
// apiErrorDetail to read. They live only inside this process: the platform
// never sees them, and nothing else reads them.
const (
	mismatchProjectHeader = "X-Provider-Mismatch-Project"
	mismatchAskedHeader   = "X-Provider-Mismatch-Region-Asked"
	mismatchRegionHeader  = "X-Provider-Mismatch-Project-Region"
)

// regionMismatchTransport finds the project's region for a refused request,
// so the diagnostic can name it. lookup is set once the client exists, since
// it makes its request through that client.
type regionMismatchTransport struct {
	next   http.RoundTripper
	lookup func(ctx context.Context, project string) (string, bool)
}

// annotate is what regionMismatchTransport does with an answer: for a 409
// PROJECT_REGION_MISMATCH on a project's route it adds the headers naming the
// project, the region asked and, when the project list shows it, the
// project's region. Anything else passes as it came. Its RoundTrip, which
// only passes the request on, is in transport.go with the other transports.
func (t *regionMismatchTransport) annotate(req *http.Request, resp *http.Response) *http.Response {
	if resp.StatusCode != http.StatusConflict || t.lookup == nil {
		return resp
	}
	// The operation's own route names the region and project, read off the
	// generated table, so a contract that moves the project routes moves
	// this too.
	id, _ := k0rdent.OperationIDFromContext(req.Context())
	if _, ok := k0rdent.Operations[id]; !ok {
		return resp
	}
	p, ok := routeOf(id).params(req.URL.EscapedPath())
	if !ok || p["region"] == "" || p["project"] == "" {
		return resp
	}
	// The body is read here and handed back unread, so the caller decodes
	// the same answer, and a read that failed fails the caller's read the
	// same way. An error body is small.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if readErr != nil {
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), failingReader{readErr}))
		return resp
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if !k0rdent.NewAPIError(resp, body).HasReason(regionMismatch) {
		return resp
	}
	asked, project := p["region"], p["project"]
	resp.Header.Set(mismatchProjectHeader, project)
	resp.Header.Set(mismatchAskedHeader, asked)
	if region, ok := t.lookup(req.Context(), project); ok {
		resp.Header.Set(mismatchRegionHeader, region)
	}
	return resp
}

// failingReader answers every read with err.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// regionMismatchAdvice is the paragraph apiErrorDetail adds to a refusal
// with that reason, or "" for any other.
func regionMismatchAdvice(apiErr *k0rdent.APIError) string {
	if apiErr.Header == nil || apiErr.Header.Get(mismatchProjectHeader) == "" {
		return ""
	}
	project, asked := apiErr.Header.Get(mismatchProjectHeader), apiErr.Header.Get(mismatchAskedHeader)
	if region := apiErr.Header.Get(mismatchRegionHeader); region != "" {
		return fmt.Sprintf("Project %q is in region %q, and this request named %q. A project's region "+
			"never changes, so set `region` to %q on this resource, or in the provider block, and plan "+
			"again.", project, region, asked, region)
	}
	// The lookup read the project list, or failed to, and it didn't say, so
	// pointing at a read of the same list would go nowhere. The read naming the
	// organization tells two organizations' projects apart, and falls back
	// to the list filtered to that organization for a credential scoped to
	// the project.
	return fmt.Sprintf("Project %q is locked to another region than %q, and neither the platform's answer "+
		"nor the project list told the provider which. Read it as `region` from the `%s` data source with "+
		"`organization_id` set. A project's region never changes, so set `region` to it on this resource, "+
		"or in the provider block.", project, asked, ResourceName(projectDataSourceSuffix))
}

// projectRegionMemo is each project's region as this provider process read
// it, so a plan with many resources in one project reads the project list
// once. A failed read isn't kept.
type projectRegionMemo struct {
	mu   sync.Mutex
	seen map[string]projectRegionAnswer
}

type projectRegionAnswer struct {
	region string
	known  bool
}

// projectRegion is the region project is locked to, when the project list
// shows it in exactly one region. It's false for a project the list doesn't
// show, one made before regions were required, and one whose id two
// organizations hold in different regions: the platform decides those.
func (m *Meta) projectRegion(ctx context.Context, project string) (string, bool) {
	m.projectRegions.mu.Lock()
	defer m.projectRegions.mu.Unlock()
	if got, ok := m.projectRegions.seen[project]; ok {
		return got.region, got.known
	}
	search := project
	params := k0rdent.ListProjectsParams{Search: &search}
	rows, err := readProjects(ctx, m, params)
	if err != nil {
		tflog.Debug(ctx, "could not read the project list for the project's region",
			map[string]any{"project": project, "error": redactDiagnosticText(err.Error())})
		return "", false
	}
	answer := projectRegionAnswer{}
	for _, p := range rows {
		if deref(p.Id) != project || deref(p.Region) == "" {
			continue
		}
		if answer.known && answer.region != deref(p.Region) {
			answer = projectRegionAnswer{}
			break
		}
		answer = projectRegionAnswer{region: deref(p.Region), known: true}
	}
	if m.projectRegions.seen == nil {
		m.projectRegions.seen = map[string]projectRegionAnswer{}
	}
	m.projectRegions.seen[project] = answer
	return answer.region, answer.known
}

// checkRegionMatchesProject refuses at plan time a create in a region its
// project isn't in, when the project list says where the project is. Only a
// create asks: a resource that exists is in its project's region already, or
// every read of it would have been refused.
func checkRegionMatchesProject(ctx context.Context, meta *Meta, region, project string, diags *diag.Diagnostics) {
	if meta == nil || meta.Client == nil || region == "" || project == "" {
		return
	}
	home, ok := meta.projectRegion(ctx, project)
	if !ok || home == region {
		return
	}
	// Local: decided from the project list, which the platform answered.
	diags.AddAttributeError(path.Root("region"), "This resource's region isn't its project's",
		redactDiagnosticText(fmt.Sprintf("Project %q is in region %q, and this resource would be created in "+
			"%q. A project is locked to one region, and the platform refuses every request for it in another, "+
			"so set `region` to %q here, or in the provider block.", project, home, region, home)))
}
