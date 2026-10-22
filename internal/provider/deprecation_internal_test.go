package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/action"
	aschema "github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/list"
	lschema "github.com/hashicorp/terraform-plugin-framework/list/schema"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
)

// A deprecation warns for the published period, which
// docs/public-compatibility.md sets as until the next major version. Every
// deprecation in the registered schemas has to name that version, worked out
// from the latest release CHANGELOG.md records.
func TestEveryDeprecationNamesTheVersionItIsRemovedIn(t *testing.T) {
	changelog, err := os.ReadFile(filepath.Join("..", "..", "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, gaps := Deprecations(context.Background(), ReleasedMajor(changelog))
	if len(gaps) > 0 {
		t.Fatalf("%d deprecation(s) that don't follow the policy:\n%s", len(gaps), strings.Join(gaps, "\n"))
	}
}

func TestReleasedMajorReadsBothHeadingStyles(t *testing.T) {
	for body, want := range map[string]int{
		"# Changelog\n\n## [Unreleased]\n\n## [0.1.0-beta] - planned, not published\n":      0,
		"## [Unreleased]\n\n## [0.3.0] - 2026-10-20\n":                                      0,
		"## 1.1.0 (Unreleased)\n\n## 1.0.0 (November 3, 2026)\n\n## [0.3.0] - 2026-10-20\n": 1,
		"## 2.0.0-beta.1 (January 5, 2027)\n\n## 1.4.2 (December 1, 2026)\n":                2,
		// Not a release: the version a section will be, and a reserved heading.
		"## 3.0.0 (Unreleased)\n\n## [2.0.0] - planned, not published\n": 0,
	} {
		if got := ReleasedMajor([]byte(body)); got != want {
			t.Errorf("ReleasedMajor(%q) = %d, want %d", body, got, want)
		}
	}
}

// Until 1.0 ships the next major is v1.0.0, so a message has to name exactly
// that. Naming v2.0.0 promises a longer window than the policy keeps, and
// naming the current major promises a removal the policy forbids.
func TestDeprecationGapsHoldTheMessageToTheNextMajor(t *testing.T) {
	cases := []struct {
		released int
		message  string
		want     string // a fragment of the gap, or "" for none
	}{
		{0, "Use `new` instead. `old` is removed in v1.0.0.", ""},
		{0, "Use `new` instead. It is Removed in V1.0.0.", ""},
		{0, "Use `new` instead. `old` is removed in v2.0.0.", "too far"},
		{0, "Use `new` instead. `old` is removed in v0.4.0.", "doesn't name"},
		{0, "Use `new` instead. It will be removed in a future version.", "doesn't name"},
		{1, "Use `new` instead. `old` is removed in v2.0.0.", ""},
		{1, "Use `new` instead. `old` is removed in v1.0.0.", "already out"},
		{3, "Use `new` instead. `old` is removed in v2.0.0.", "already out"},
	}
	for _, c := range cases {
		attrs := map[string]schema.Attribute{"old": schema.StringAttribute{Optional: true, DeprecationMessage: c.message}}
		_, gaps := deprecationGaps(surfaceForTest(attrs, nil, ""), c.released)
		switch {
		case c.want == "" && len(gaps) > 0:
			t.Errorf("released %d, %q: %v", c.released, c.message, gaps)
		case c.want != "" && (len(gaps) != 1 || !strings.Contains(gaps[0], c.want)):
			t.Errorf("released %d, %q: want a gap containing %q, got %v", c.released, c.message, c.want, gaps)
		}
	}
}

func TestDeprecationGapsReadEveryLevel(t *testing.T) {
	attrs := map[string]schema.Attribute{
		"kept": schema.StringAttribute{Optional: true},
		"old": schema.StringAttribute{
			Optional:           true,
			DeprecationMessage: "Use `new` instead. `old` is removed in v1.0.0.",
		},
		"vague": schema.StringAttribute{
			Optional:           true,
			DeprecationMessage: "Use `new` instead. It will be removed in a future version.",
		},
		"settings": schema.SingleNestedAttribute{
			Optional: true,
			Attributes: map[string]schema.Attribute{
				"inner": schema.BoolAttribute{Optional: true, DeprecationMessage: "Going away."},
			},
		},
		"rules": schema.ListNestedAttribute{
			Optional: true,
			NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
				"port": schema.Int64Attribute{Optional: true, DeprecationMessage: "Removed in v0.4.0."},
			}},
		},
	}
	blocks := map[string]schema.Block{
		"legacy": schema.SingleNestedBlock{DeprecationMessage: "Use `settings`."},
	}
	count, gaps := deprecationGaps(surfaceForTest(attrs, blocks, "Use "+ResourceName("other")+". Removed in v1.0.0."), 0)
	if count != 6 {
		t.Errorf("counted %d deprecations, want 6: the surface and five attributes or blocks", count)
	}
	want := []string{
		"resource " + ResourceName("example") + ".legacy",
		"resource " + ResourceName("example") + ".rules.port",
		"resource " + ResourceName("example") + ".settings.inner",
		"resource " + ResourceName("example") + ".vague",
	}
	if len(gaps) != len(want) {
		t.Fatalf("gaps:\n%s", strings.Join(gaps, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(gaps[i], w+": ") {
			t.Errorf("gap %d is %q, want it to start %q", i, gaps[i], w)
		}
	}
}

func TestDeprecationGapsReadTheSurfaceItself(t *testing.T) {
	_, gaps := deprecationGaps(surfaceForTest(nil, nil, "This resource is going away."), 0)
	if len(gaps) != 1 || !strings.HasPrefix(gaps[0], "resource "+ResourceName("example")+": ") {
		t.Fatalf("gaps: %q", gaps)
	}
}

// The provider registers no list resource or action yet. When it does, their
// deprecations are held to the same policy, so the walk reads them through
// the same interfaces Terraform does.
func TestDeprecationSurfacesIncludeListResourcesAndActions(t *testing.T) {
	surfaces := listAndActionSurfaces(context.Background(), stubProvider{})
	if len(surfaces) != 2 {
		t.Fatalf("want a list resource and an action, got %d surfaces", len(surfaces))
	}
	var gaps []string
	count := 0
	for _, s := range surfaces {
		n, g := deprecationGaps(s, 0)
		count += n
		gaps = append(gaps, g...)
	}
	if count != 2 || len(gaps) != 2 {
		t.Fatalf("counted %d, gaps %v", count, gaps)
	}
	if !strings.HasPrefix(gaps[0], "list resource "+ResourceName("stub")+".filter: ") ||
		!strings.HasPrefix(gaps[1], "action "+ResourceName("stub")+": ") {
		t.Fatalf("gaps: %v", gaps)
	}
}

func surfaceForTest(attrs map[string]schema.Attribute, blocks map[string]schema.Block, deprecation string) docSurface {
	return docSurface{
		kind: "resource", name: ResourceName("example"),
		attributes: reflect.ValueOf(attrs), blocks: reflect.ValueOf(blocks),
		deprecation: deprecation,
	}
}

type stubProvider struct{ fwprovider.Provider }

func (stubProvider) ListResources(context.Context) []func() list.ListResource {
	return []func() list.ListResource{func() list.ListResource { return stubList{} }}
}

func (stubProvider) Actions(context.Context) []func() action.Action {
	return []func() action.Action{func() action.Action { return stubAction{} }}
}

type stubList struct{}

func (stubList) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName("stub")
}

func (stubList) ListResourceConfigSchema(_ context.Context, _ list.ListResourceSchemaRequest, resp *list.ListResourceSchemaResponse) {
	resp.Schema = lschema.Schema{Attributes: map[string]lschema.Attribute{
		"filter": lschema.StringAttribute{Optional: true, DeprecationMessage: "Going away."},
	}}
}

func (stubList) List(context.Context, list.ListRequest, *list.ListResultsStream) {}

type stubAction struct{}

func (stubAction) Metadata(_ context.Context, _ action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = ResourceName("stub")
}

func (stubAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = aschema.Schema{DeprecationMessage: "Going away."}
}

func (stubAction) Invoke(context.Context, action.InvokeRequest, *action.InvokeResponse) {}
