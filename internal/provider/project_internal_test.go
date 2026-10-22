package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// Import takes `organization/project`, or a bare id whose organization is
// looked up. Anything else is refused before it reaches a URL.
func TestProjectImportIDForms(t *testing.T) {
	cases := []struct {
		raw     string
		org, id string
		ok      bool
	}{
		{"org-ml/cp-production", "org-ml", "cp-production", true},
		{"cp-production", "", "cp-production", true},
		{"proj-a1b2c3d4", "", "proj-a1b2c3d4", true},
		{"", "", "", false},
		{"/cp-production", "", "", false},
		{"org-ml/", "", "", false},
		{"us-texas-1/org-ml/cp-production", "", "", false},

		// Both halves become segments of the request URL. The generated client
		// escapes a slash and not a dot, and the request builder finishes with
		// RFC 3986 reference resolution, so `..` in either half collapses the
		// path onto a different route. An import id writes straight to state
		// without passing the schema validator, so this is the only place that
		// catches it.
		{"..", "", "", false},
		{"../cp-production", "", "", false},
		{"org-ml/..", "", "", false},
		{"./cp-production", "", "", false},
		{"org-ml/.", "", "", false},
		{"org-ml/cp production", "", "", false},
		{"org-ml/CP-Production", "", "", false},
		{"org-ml/cp?x=1", "", "", false},
		{"org-ml/cp#frag", "", "", false},
	}
	for _, c := range cases {
		org, id, ok := splitProjectImportID(c.raw)
		if ok != c.ok || org != c.org || id != c.id {
			t.Errorf("%q: got %q %q %v, want %q %q %v", c.raw, org, id, ok, c.org, c.id, c.ok)
		}
	}
}

// Every field on Project is a pointer, because the whole type is readOnly and
// the generated client accepts a nil silently. The organization is the one
// field this resource cannot function without, and it is also the one the
// caller definitely knows, because it was in the URL it just called.
func TestProjectStateKeepsTheOrganizationWhenTheResponseOmitsIt(t *testing.T) {
	prior := projectModel{OrganizationID: types.StringValue("org-ml")}

	out := projectStateFrom(k0rdent.Project{Id: ptr("cp-production")}, prior)
	if out.OrganizationID.ValueString() != "org-ml" {
		t.Errorf("organization %q, want it kept from the request", out.OrganizationID.ValueString())
	}
	if !out.Status.IsNull() {
		t.Errorf("status %v, want null when the platform reported none", out.Status)
	}

	// And it is overridden when the platform does say, because the platform is
	// the authority on where the project actually ended up.
	out = projectStateFrom(k0rdent.Project{
		Id:             ptr("cp-production"),
		OrganizationId: ptr("org-elsewhere"),
	}, prior)
	if out.OrganizationID.ValueString() != "org-elsewhere" {
		t.Errorf("organization %q, want the platform's answer", out.OrganizationID.ValueString())
	}
}

// suspended and disabled are steady states the platform can hold a project in,
// and state has to report the status the platform gives. Only active is seen
// in an acceptance run, so this is the one check that a status other than
// active reaches state as it is. No wait reads these: a create doesn't wait,
// and the delete wait is covered by the lifecycle tests.
func TestProjectStateReportsEverySteadyStatus(t *testing.T) {
	for _, status := range []k0rdent.ProjectStatus{
		k0rdent.ProjectStatusActive,
		k0rdent.ProjectStatusSuspended,
		k0rdent.ProjectStatusDisabled,
		k0rdent.ProjectStatusDeleting,
	} {
		out := projectStateFrom(k0rdent.Project{Id: ptr("p"), Status: &status}, projectModel{})
		if out.Status.ValueString() != string(status) {
			t.Errorf("status %q, want %q", out.Status.ValueString(), status)
		}
	}
}
