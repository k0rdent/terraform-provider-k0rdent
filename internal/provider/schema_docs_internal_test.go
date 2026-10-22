package provider

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/dynamicplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float32planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int32planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/numberplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The reference has to say, on every attribute, what its schema already
// enforces. The list comes from the registered schemas, so an attribute added
// without its facts fails here rather than reaching the registry page bare.
func TestEveryAttributeDescriptionStatesItsSchema(t *testing.T) {
	if gaps := DocumentationGaps(context.Background()); len(gaps) > 0 {
		t.Fatalf("%d gaps in the per-attribute reference:\n%s", len(gaps), strings.Join(gaps, "\n"))
	}
}

// Every page links the pages a reader of it wants next, and every link lands
// on a page something registers.
func TestEveryPageLinksItsRelated(t *testing.T) {
	if gaps := RelatedGaps(context.Background()); len(gaps) > 0 {
		t.Fatalf("%d gaps in the Related sections:\n%s", len(gaps), strings.Join(gaps, "\n"))
	}
}

// A surface with a timeouts block gets a Timeouts section naming each
// operation the block takes with the default the code applies, and a surface
// without one gets none.
func TestEveryTimeoutsBlockGetsASection(t *testing.T) {
	ctx := context.Background()
	sections := PageSections(ctx)
	def := "(default `" + shortDuration(DefaultOperationTimeout) + "`)"
	withBlock := 0
	for _, s := range docSurfaces(ctx) {
		if s.page == "" {
			continue
		}
		ops := timeoutOperationsOf(s)
		got := sections[s.page]
		if len(ops) == 0 {
			if strings.Contains(got, "## Timeouts") {
				t.Errorf("%s has no timeouts block and still gets a Timeouts section", s.page)
			}
			continue
		}
		withBlock++
		if !strings.Contains(got, "## Timeouts\n") {
			t.Errorf("%s has a timeouts block and no Timeouts section", s.page)
			continue
		}
		// A cluster's and an instance group's create and delete build and
		// free hardware, and default to the long timeout.
		hardware := s.kind == "resource" &&
			(s.name == ResourceName(clusterResourceSuffix) || s.name == ResourceName(instanceGroupResourceSuffix))
		for _, op := range ops {
			want := def
			if hardware && (op == "create" || op == "delete") {
				want = "(default `" + shortDuration(DefaultLongOperationTimeout) + "`)"
			}
			if !strings.Contains(got, "- `"+op+"` "+want) {
				t.Errorf("%s: the Timeouts section doesn't give %s its default %s:\n%s", s.page, op, want, got)
			}
		}
	}
	// Nine resources and the ephemeral kubeconfig, today.
	if withBlock < 10 {
		t.Fatalf("only %d surfaces have a timeouts block; the walk has stopped finding them", withBlock)
	}
}

// 30m, not 30m0s, and an hour as 1h.
func TestShortDuration(t *testing.T) {
	for d, want := range map[string]string{"30m0s": "30m", "1h0m0s": "1h", "1h30m0s": "1h30m", "45s": "45s"} {
		parsed, err := time.ParseDuration(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := shortDuration(parsed); got != want {
			t.Errorf("shortDuration(%s) = %s, want %s", d, got, want)
		}
	}
}

// The fact pass tells an unconditional RequiresReplace from a conditional one
// by the description the framework gives it. An upgrade that rewords it would
// turn every **Forces replacement** into a **Can force replacement** quoting
// the framework, so this pins the text for every kind of attribute.
func TestRequiresReplaceDescriptionIsTheFrameworks(t *testing.T) {
	ctx := context.Background()
	for name, m := range map[string]describer{
		"bool":    boolplanmodifier.RequiresReplace(),
		"dynamic": dynamicplanmodifier.RequiresReplace(),
		"float32": float32planmodifier.RequiresReplace(),
		"float64": float64planmodifier.RequiresReplace(),
		"int32":   int32planmodifier.RequiresReplace(),
		"int64":   int64planmodifier.RequiresReplace(),
		"list":    listplanmodifier.RequiresReplace(),
		"map":     mapplanmodifier.RequiresReplace(),
		"number":  numberplanmodifier.RequiresReplace(),
		"object":  objectplanmodifier.RequiresReplace(),
		"set":     setplanmodifier.RequiresReplace(),
		"string":  stringplanmodifier.RequiresReplace(),
	} {
		if got := m.MarkdownDescription(ctx); got != requiresReplaceDescription {
			t.Errorf("%splanmodifier.RequiresReplace describes itself as %q, not %q", name, got, requiresReplaceDescription)
		}
	}
}

// A description can carry the marker and still mislead, or be too short to
// help. Each of these passes an emptiness check and a search for the words.
func TestMisleadingOrBareDescriptionsFail(t *testing.T) {
	ctx := context.Background()
	always := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	sometimes := []planmodifier.String{stringplanmodifier.RequiresReplaceIf(
		func(context.Context, planmodifier.StringRequest, *stringplanmodifier.RequiresReplaceIfFuncResponse) {},
		"replaced when the pool shrinks", "replaced when the pool shrinks")}
	for name, tc := range map[string]struct {
		attr schema.StringAttribute
		want string
	}{
		"denies a replacement it forces": {schema.StringAttribute{
			MarkdownDescription: "The pool to draw from. Changing it never forces replacement. **Forces replacement**.",
			PlanModifiers:       always}, "its prose says it doesn't"},
		"calls a conditional replacement unconditional": {schema.StringAttribute{
			MarkdownDescription: "Users on every host. Immutable. **Can force replacement**: replaced when the pool shrinks.",
			PlanModifiers:       sometimes}, "says always or never"},
		"claims a replacement that doesn't happen": {schema.StringAttribute{
			MarkdownDescription: "Human-friendly name. Immutable once set."}, "its prose says it replaces"},
		"one word": {schema.StringAttribute{MarkdownDescription: "Id."}, "too short"},
	} {
		gaps := attributeGaps(ctx, "test_resource.attr", reflect.ValueOf(tc.attr), false)
		if !strings.Contains(strings.Join(gaps, "\n"), tc.want) {
			t.Errorf("%s: want a gap saying %q, got %q", name, tc.want, gaps)
		}
	}
}

// Constraint sentences are written for a tenant. The framework's composed
// validators describe themselves in a grammar nobody would write ("at least 1
// elements", "satisfy all validations: a + b"), and the timeouts library
// offers "-1.5h" as an example of a value the provider would quietly replace
// with its default.
func TestConstraintSentencesReadAsEnglish(t *testing.T) {
	ctx := context.Background()
	unreadable := regexp.MustCompile(`\b1 elements\b| \+ |satisfy all validations|-1\.5h`)
	for _, s := range docSurfaces(ctx) {
		for _, m := range []reflect.Value{s.attributes, s.blocks} {
			walkDocumented(m, s.kind+" "+s.name+".", false, func(path string, v reflect.Value, _ bool) {
				for _, d := range validatorDescriptions(ctx, v) {
					if unreadable.MatchString(d) {
						t.Errorf("%s: %q", path, d)
					}
				}
			})
		}
	}
}

// A timeout of zero or less would fall back to the default without a word
// (withTimeout), so the plan refuses it.
func TestTimeoutsRefuseNonPositiveDurations(t *testing.T) {
	ctx := context.Background()
	var sr resource.SchemaResponse
	newSubnetResource().Schema(ctx, resource.SchemaRequest{}, &sr)
	block, ok := sr.Schema.Blocks["timeouts"].(schema.SingleNestedBlock)
	if !ok {
		t.Fatal("the subnet has no timeouts block")
	}
	create, ok := block.Attributes["create"].(schema.StringAttribute)
	if !ok {
		t.Fatal("the timeouts block has no create")
	}
	for value, wantError := range map[string]bool{"45m": false, "2h30m": false, "0s": true, "-1h": true, "soon": true} {
		var resp validator.StringResponse
		for _, v := range create.Validators {
			v.ValidateString(ctx, validator.StringRequest{
				Path: path.Root("timeouts").AtName("create"), ConfigValue: types.StringValue(value),
			}, &resp)
		}
		if resp.Diagnostics.HasError() != wantError {
			t.Errorf("timeouts.create = %q: error %v, want %v (%v)", value, resp.Diagnostics.HasError(), wantError, resp.Diagnostics)
		}
	}
}
