package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

func scopePtr(s string) *string { return &s }

func scopeStr(s *string) types.String {
	if s == nil {
		return types.StringNull()
	}
	return types.StringValue(*s)
}

func TestScopeModifierResolvesAndReplaces(t *testing.T) {
	tests := []struct {
		name string

		providerDefault string
		config          *string // the attribute's value in configuration
		configUnknown   bool    // the configuration has a value that resolves at apply
		state           *string // the attribute's value in prior state
		hasState        bool    // false = create
		hasPlan         bool    // false = destroy

		wantPlan    *string // expected resolved plan value; nil = left untouched
		wantReplace bool
	}{
		{
			name:            "create defaults from the provider block",
			providerDefault: "us-texas-1",
			hasPlan:         true,
			wantPlan:        scopePtr("us-texas-1"),
		},
		{
			name:            "create honours an explicit value over the provider default",
			providerDefault: "us-texas-1",
			config:          scopePtr("eu-west-1"),
			hasPlan:         true,
			wantPlan:        scopePtr("eu-west-1"),
		},
		{
			name:            "steady state produces no replacement",
			providerDefault: "us-texas-1",
			state:           scopePtr("us-texas-1"),
			hasState:        true,
			hasPlan:         true,
			wantPlan:        scopePtr("us-texas-1"),
		},
		{
			// Writing down an inherited value must not destroy the resource.
			name:            "setting the attribute to the value it already inherited is not a change",
			providerDefault: "us-texas-1",
			config:          scopePtr("us-texas-1"),
			state:           scopePtr("us-texas-1"),
			hasState:        true,
			hasPlan:         true,
			wantPlan:        scopePtr("us-texas-1"),
		},
		{
			name:            "explicitly moving the resource replaces it",
			providerDefault: "us-texas-1",
			config:          scopePtr("eu-west-1"),
			state:           scopePtr("us-texas-1"),
			hasState:        true,
			hasPlan:         true,
			wantPlan:        scopePtr("eu-west-1"),
			wantReplace:     true,
		},
		{
			// The defect this file exists for: the change used to be invisible
			// and every resource was quietly retargeted.
			name:            "changing the provider default is visible in the plan",
			providerDefault: "eu-west-1",
			state:           scopePtr("us-texas-1"),
			hasState:        true,
			hasPlan:         true,
			wantPlan:        scopePtr("eu-west-1"),
			wantReplace:     true,
		},
		{
			// Older state, or -refresh=false. Static ForceNew destroys live
			// infrastructure here.
			name:            "null prior state does not force replacement",
			providerDefault: "us-texas-1",
			hasState:        true,
			hasPlan:         true,
			wantPlan:        scopePtr("us-texas-1"),
		},
		{
			name:            "a destroy plan is left alone",
			providerDefault: "us-texas-1",
			state:           scopePtr("us-texas-1"),
			hasState:        true,
			wantPlan:        nil,
		},
		{
			name:     "no configured value and no default defers to resource validation",
			hasPlan:  true,
			wantPlan: nil,
		},
		{
			// Defaulting an unknown plans against a guess, and Terraform
			// reports an inconsistent final plan when the real value arrives.
			name:            "an unresolved configured value is not defaulted on create",
			providerDefault: "us-texas-1",
			configUnknown:   true,
			hasPlan:         true,
			wantPlan:        nil,
		},
		{
			// The same guess, compared against live state, destroys the
			// resource over a value nobody wrote.
			name:            "an unresolved configured value does not force replacement",
			providerDefault: "us-texas-1",
			configUnknown:   true,
			state:           scopePtr("eu-west-1"),
			hasState:        true,
			hasPlan:         true,
			wantPlan:        nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := scopeStr(tc.config)
			if tc.configUnknown {
				config = types.StringUnknown()
			}
			out := resolveScopeValue(
				config,
				scopeStr(tc.state),
				tc.providerDefault,
				!tc.hasPlan,
				!tc.hasState,
			)

			if tc.wantPlan == nil {
				if out.Write {
					t.Errorf("wrote %v into the plan, want the attribute left alone", out.Value)
				}
			} else {
				if !out.Write {
					t.Fatalf("left the attribute alone, want %q written into the plan", *tc.wantPlan)
				}
				if want := types.StringValue(*tc.wantPlan); !out.Value.Equal(want) {
					t.Errorf("plan value = %v, want %v", out.Value, want)
				}
			}
			if out.Replace != tc.wantReplace {
				t.Errorf("Replace = %v, want %v", out.Replace, tc.wantReplace)
			}
		})
	}
}

// An import that names a scope the provider is not configured for survives only
// if the resource block repeats it. Silence here is a destroyed cluster on the
// next plan.
func TestImportingIntoAnotherScopeWarns(t *testing.T) {
	t.Parallel()

	meta := &Meta{Region: "us-texas-1", Project: "cp-production"}

	for _, tc := range []struct {
		name            string
		region, project string
		wantWarning     bool
		wantContains    []string
	}{
		{name: "the provider's own scope needs nothing", region: "us-texas-1", project: "cp-production"},
		{name: "the bare id form needs nothing"},
		{
			name:         "another region",
			region:       "eu-west-1",
			project:      "cp-production",
			wantWarning:  true,
			wantContains: []string{`region  = "eu-west-1"`},
		},
		{
			name:         "another region and project",
			region:       "eu-west-1",
			project:      "cp-example",
			wantWarning:  true,
			wantContains: []string{`region  = "eu-west-1"`, `project = "cp-example"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Driven through setImportedScope, not the helper, so deleting the
			// call fails this test rather than leaving it green.
			var diags diag.Diagnostics
			state := testClusterState(t, "my-cluster", "")
			setImportedScope(context.Background(), meta, tc.region, tc.project, &state, &diags)
			if diags.HasError() {
				t.Fatalf("import produced errors: %v", diags.Errors())
			}

			if got := diags.WarningsCount(); (got > 0) != tc.wantWarning {
				t.Fatalf("warnings = %d, want warning: %v", got, tc.wantWarning)
			}
			if !tc.wantWarning {
				return
			}
			detail := diags.Warnings()[0].Detail()
			for _, want := range tc.wantContains {
				if !strings.Contains(detail, want) {
					t.Errorf("warning does not tell the user to add %s:\n%s", want, detail)
				}
			}
		})
	}
}

// The response does not echo scope, so a refresh must keep what state has.
func TestARefreshDoesNotBlankTheScopeAttributes(t *testing.T) {
	t.Parallel()

	prior := clusterModel{
		Region:  types.StringValue("us-texas-1"),
		Project: types.StringValue("cp-production"),
	}

	got, diags := clusterStateFrom(context.Background(), k0rdent.Cluster{Id: "tf-demo"}, prior, defaultTags{})
	if diags.HasError() {
		t.Fatalf("mapping produced errors: %v", diags.Errors())
	}
	if got.Region.ValueString() != "us-texas-1" {
		t.Errorf("region = %v, want it carried from prior state", got.Region)
	}
	if got.Project.ValueString() != "cp-production" {
		t.Errorf("project = %v, want it carried from prior state", got.Project)
	}
}

func TestARefreshDoesNotBlankTheInstanceGroupScope(t *testing.T) {
	t.Parallel()

	prior := instanceGroupModel{
		Region:  types.StringValue("us-texas-1"),
		Project: types.StringValue("cp-production"),
	}

	got, diags := instanceGroupStateFrom(context.Background(), k0rdent.InstanceGroup{
		Id:          "tf-pool",
		ClusterType: testPinnedRef,
	}, prior)
	if diags.HasError() {
		t.Fatalf("mapping produced errors: %v", diags.Errors())
	}
	if got.Region.ValueString() != "us-texas-1" {
		t.Errorf("region = %v, want it carried from prior state", got.Region)
	}
	if got.Project.ValueString() != "cp-production" {
		t.Errorf("project = %v, want it carried from prior state", got.Project)
	}
}

// errorSummaries lists the errors by summary, which is what a reader of the
// plan output sees first.
func errorSummaries(diags diag.Diagnostics) []string {
	var out []string
	for _, d := range diags.Errors() {
		out = append(out, d.Summary())
	}
	return out
}

func TestScopeResolution(t *testing.T) {
	t.Parallel()

	configured := &Meta{Region: "us-texas-1", Project: "cp-production"}

	for _, tc := range []struct {
		name            string
		meta            *Meta
		region, project types.String

		want       clusterTarget
		wantErrors []string // summaries, in order; nil means it resolves
	}{
		{
			name:    "nothing set on the block takes the provider's scope",
			meta:    configured,
			region:  types.StringNull(),
			project: types.StringNull(),
			want:    clusterTarget{region: "us-texas-1", project: "cp-production"},
		},
		{
			name:    "the block's own values win over the provider's",
			meta:    configured,
			region:  types.StringValue("eu-west-1"),
			project: types.StringValue("cp-example"),
			want:    clusterTarget{region: "eu-west-1", project: "cp-example"},
		},
		{
			name:    "each half falls back on its own",
			meta:    configured,
			region:  types.StringNull(),
			project: types.StringValue("cp-example"),
			want:    clusterTarget{region: "us-texas-1", project: "cp-example"},
		},
		{
			// Unknown during plan and empty from older state both mean the
			// block has nothing to say yet.
			name:    "unknown and empty values fall back to the provider",
			meta:    configured,
			region:  types.StringUnknown(),
			project: types.StringValue(""),
			want:    clusterTarget{region: "us-texas-1", project: "cp-production"},
		},
		{
			name:       "a region that is not a path segment is refused",
			meta:       configured,
			region:     types.StringValue("../admin"),
			project:    types.StringNull(),
			wantErrors: []string{"Invalid region"},
		},
		{
			// v1.3.0-dev.4: on an all-in-one install compute runs under
			// `global`, and the regions list says it hosts projects there.
			name:    "the global tier is a region where it runs compute",
			meta:    configured,
			region:  types.StringValue("global"),
			project: types.StringNull(),
			want:    clusterTarget{region: "global", project: "cp-production"},
		},
		{
			name:       "a project that is not a path segment is refused",
			meta:       configured,
			region:     types.StringNull(),
			project:    types.StringValue("a/b"),
			wantErrors: []string{"Invalid project"},
		},
		{
			name:       "no region anywhere names where to set one",
			meta:       &Meta{Project: "cp-production"},
			region:     types.StringNull(),
			project:    types.StringNull(),
			wantErrors: []string{"No region configured"},
		},
		{
			name:       "no project anywhere names where to set one",
			meta:       &Meta{Region: "us-texas-1"},
			region:     types.StringNull(),
			project:    types.StringNull(),
			wantErrors: []string{"No project configured"},
		},
		{
			// Both at once, so fixing one does not uncover the other on the
			// next run.
			name:       "no scope anywhere names both halves",
			meta:       &Meta{},
			region:     types.StringNull(),
			project:    types.StringNull(),
			wantErrors: []string{"No region configured", "No project configured"},
		},
		{
			// A bad region does not hide the missing project behind it.
			name:       "an invalid region and no project are both reported",
			meta:       &Meta{Region: "us-texas-1"},
			region:     types.StringValue("US-WEST"),
			project:    types.StringNull(),
			wantErrors: []string{"No project configured", "Invalid region"},
		},
		{
			name:       "a missing region and an invalid project are both reported",
			meta:       &Meta{Project: "cp-production"},
			region:     types.StringNull(),
			project:    types.StringValue("a/b"),
			wantErrors: []string{"No region configured", "Invalid project"},
		},
		{
			name:    "a block that sets both needs nothing from the provider",
			meta:    &Meta{},
			region:  types.StringValue("eu-west-1"),
			project: types.StringValue("cp-example"),
			want:    clusterTarget{region: "eu-west-1", project: "cp-example"},
		},
		{
			name:       "an unconfigured provider is refused before anything else",
			meta:       nil,
			region:     types.StringValue("eu-west-1"),
			project:    types.StringValue("cp-example"),
			wantErrors: []string{"The provider was not configured"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			got, ok := resolveScope(tc.meta, tc.region, tc.project, &diags)

			if gotErrors := errorSummaries(diags); strings.Join(gotErrors, "|") != strings.Join(tc.wantErrors, "|") {
				t.Fatalf("errors = %q, want %q", gotErrors, tc.wantErrors)
			}
			if ok != (tc.wantErrors == nil) {
				t.Fatalf("ok = %v with errors %q", ok, tc.wantErrors)
			}
			if got != tc.want {
				t.Errorf("target = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Region-wide catalogs need a region and nothing else, so a provider with no
// project must still read them.
func TestRegionResolutionNeedsNoProject(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		meta       *Meta
		region     types.String
		want       string
		wantErrors []string
	}{
		{name: "the provider's region", meta: &Meta{Region: "us-texas-1"}, region: types.StringNull(), want: "us-texas-1"},
		{name: "the block's region wins", meta: &Meta{Region: "us-texas-1"}, region: types.StringValue("eu-west-1"), want: "eu-west-1"},
		{name: "no region anywhere", meta: &Meta{}, region: types.StringNull(), wantErrors: []string{"No region configured"}},
		{name: "an invalid region", meta: &Meta{}, region: types.StringValue("US-WEST"), wantErrors: []string{"Invalid region"}},
		{name: "the global tier", meta: &Meta{}, region: types.StringValue("global"), want: "global"},
		{name: "an unconfigured provider", meta: nil, region: types.StringValue("eu-west-1"), wantErrors: []string{"The provider was not configured"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			got, ok := resolveRegion(tc.meta, tc.region, &diags)

			if gotErrors := errorSummaries(diags); strings.Join(gotErrors, "|") != strings.Join(tc.wantErrors, "|") {
				t.Fatalf("errors = %q, want %q", gotErrors, tc.wantErrors)
			}
			if ok != (tc.wantErrors == nil) || got != tc.want {
				t.Errorf("region = %q, %v, want %q", got, ok, tc.want)
			}
		})
	}
}

// Terraform configures the provider before it reads anything, and Configure
// either hands over a Meta or fails the run. A data source without one is a
// provider bug, and it has to surface as an error, not as a crash that takes
// the plugin process down with it.
func TestEveryDataSourceRefusesToReadUnconfigured(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, newDataSource := range (&aiProvider{}).DataSources(ctx) {
		ds := newDataSource()
		var md datasource.MetadataResponse
		ds.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)

		t.Run(md.TypeName, func(t *testing.T) {
			var s datasource.SchemaResponse
			ds.Schema(ctx, datasource.SchemaRequest{}, &s)
			typ, ok := s.Schema.Type().TerraformType(ctx).(tftypes.Object)
			if !ok {
				t.Fatal("the schema is not an object")
			}
			vals := map[string]tftypes.Value{}
			for name, at := range typ.AttributeTypes {
				vals[name] = tftypes.NewValue(at, nil)
			}
			raw := tftypes.NewValue(typ, vals)
			resp := datasource.ReadResponse{State: tfsdk.State{Raw: raw, Schema: s.Schema}}

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("read panicked: %v", r)
				}
			}()
			ds.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Raw: raw, Schema: s.Schema}}, &resp)

			if got := errorSummaries(resp.Diagnostics); len(got) != 1 || got[0] != "The provider was not configured" {
				t.Errorf("errors = %q, want only that the provider was not configured", got)
			}
		})
	}
}
