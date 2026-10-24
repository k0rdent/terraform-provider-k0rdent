package provider

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func tagsOf(kv ...string) types.Map {
	m := map[string]attr.Value{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = types.StringValue(kv[i+1])
	}
	return types.MapValueMust(types.StringType, m)
}

var (
	nullString = types.StringNull()
	nullTags   = types.MapNull(types.StringType)
)

// What goes on the wire for a create, and what doesn't.
func TestTenantMetadataCreate(t *testing.T) {
	for name, tc := range map[string]struct {
		plan tenantMetadata
		want metadataFields
	}{
		"nothing set leaves all three out": {
			plan: tenantMetadata{DisplayName: nullString, Description: nullString, Tags: nullTags},
		},
		"no tags on a resource that has none": {
			plan: tenantMetadata{DisplayName: types.StringValue("a"), Description: nullString},
			want: metadataFields{DisplayName: ptr("a")},
		},
		"empty values are sent as written": {
			plan: tenantMetadata{DisplayName: types.StringValue(""), Description: types.StringValue(""), Tags: tagsOf()},
			want: metadataFields{ptr(""), ptr(""), &map[string]string{}},
		},
		"an unknown is left out, never sent as empty": {
			plan: tenantMetadata{DisplayName: types.StringUnknown(), Description: nullString, Tags: types.MapUnknown(types.StringType)},
		},
		"set values": {
			plan: tenantMetadata{DisplayName: types.StringValue("a"), Description: types.StringValue("b"), Tags: tagsOf("team", "ml")},
			want: metadataFields{ptr("a"), ptr("b"), &map[string]string{"team": "ml"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, diags := tc.plan.create(context.Background())
			if diags.HasError() {
				t.Fatal(diags)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("create = %s, want %s", fieldsString(got), fieldsString(tc.want))
			}
		})
	}
}

// When a PATCH is needed, and what it carries.
func TestTenantMetadataPatch(t *testing.T) {
	set := tenantMetadata{DisplayName: types.StringValue("a"), Description: types.StringValue("b"), Tags: tagsOf("team", "ml")}
	for name, tc := range map[string]struct {
		plan, state tenantMetadata
		want        metadataFields
		changed     bool
	}{
		"unchanged values need no PATCH, and are still complete": {
			plan: set, state: set,
			want: metadataFields{ptr("a"), ptr("b"), &map[string]string{"team": "ml"}},
		},
		"nothing set on either side": {
			plan:  tenantMetadata{DisplayName: nullString, Description: nullString, Tags: nullTags},
			state: tenantMetadata{DisplayName: nullString, Description: nullString, Tags: nullTags},
		},
		"a resource with no tags compares its zero maps as the same": {
			plan:  tenantMetadata{DisplayName: types.StringValue("a"), Description: nullString},
			state: tenantMetadata{DisplayName: types.StringValue("a"), Description: nullString},
			want:  metadataFields{DisplayName: ptr("a")},
		},
		"a new value is sent": {
			plan:    tenantMetadata{DisplayName: types.StringValue("new"), Description: types.StringValue("b"), Tags: tagsOf("team", "ml")},
			state:   set,
			want:    metadataFields{ptr("new"), ptr("b"), &map[string]string{"team": "ml"}},
			changed: true,
		},
		"removing each from configuration clears it upstream": {
			plan:    tenantMetadata{DisplayName: nullString, Description: nullString, Tags: nullTags},
			state:   set,
			want:    metadataFields{ptr(""), ptr(""), &map[string]string{}},
			changed: true,
		},
		"removing an empty tag map still clears it": {
			plan:    tenantMetadata{DisplayName: nullString, Description: nullString, Tags: nullTags},
			state:   tenantMetadata{DisplayName: nullString, Description: nullString, Tags: tagsOf()},
			want:    metadataFields{Tags: &map[string]string{}},
			changed: true,
		},
		"setting tags to empty is a change": {
			plan:    tenantMetadata{DisplayName: nullString, Description: nullString, Tags: tagsOf()},
			state:   tenantMetadata{DisplayName: nullString, Description: nullString, Tags: tagsOf("team", "ml")},
			want:    metadataFields{Tags: &map[string]string{}},
			changed: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, changed, diags := tc.plan.patch(context.Background(), tc.state)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if changed != tc.changed {
				t.Errorf("changed = %v, want %v", changed, tc.changed)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("patch = %s, want %s", fieldsString(got), fieldsString(tc.want))
			}
		})
	}
}

// What state holds after the platform answers.
func TestTenantMetadataRead(t *testing.T) {
	for name, tc := range map[string]struct {
		prior  tenantMetadata
		answer metadataAnswer
		want   tenantMetadata
	}{
		"empty answers to a configuration that set nothing read as null": {
			prior:  tenantMetadata{DisplayName: nullString, Description: nullString, Tags: nullTags},
			answer: metadataAnswer{DisplayName: ptr(""), Description: ptr(""), Tags: tagsOf()},
			want:   tenantMetadata{DisplayName: nullString, Description: nullString, Tags: nullTags},
		},
		"empty answers to a configuration that wrote them empty stay empty": {
			prior:  tenantMetadata{DisplayName: types.StringValue(""), Description: types.StringValue(""), Tags: tagsOf()},
			answer: metadataAnswer{DisplayName: ptr(""), Description: ptr(""), Tags: tagsOf()},
			want:   tenantMetadata{DisplayName: types.StringValue(""), Description: types.StringValue(""), Tags: tagsOf()},
		},
		"tags left out of the answer keep a configured empty map": {
			prior:  tenantMetadata{DisplayName: nullString, Description: nullString, Tags: tagsOf()},
			answer: metadataAnswer{DisplayName: nil, Description: nil, Tags: nullTags},
			want:   tenantMetadata{DisplayName: nullString, Description: nullString, Tags: tagsOf()},
		},
		"tags left out of the answer are null otherwise": {
			prior:  tenantMetadata{DisplayName: nullString, Description: nullString, Tags: tagsOf("team", "ml")},
			answer: metadataAnswer{DisplayName: nil, Description: nil, Tags: nullTags},
			want:   tenantMetadata{DisplayName: nullString, Description: nullString, Tags: nullTags},
		},
		"an empty answer to configured tags reads as empty": {
			prior:  tenantMetadata{DisplayName: nullString, Description: nullString, Tags: tagsOf("team", "ml")},
			answer: metadataAnswer{DisplayName: nil, Description: nil, Tags: tagsOf()},
			want:   tenantMetadata{DisplayName: nullString, Description: nullString, Tags: tagsOf()},
		},
		"values are read as answered": {
			prior:  tenantMetadata{DisplayName: nullString, Description: types.StringValue("old"), Tags: nullTags},
			answer: metadataAnswer{DisplayName: ptr("a"), Description: ptr("b"), Tags: tagsOf("team", "ml")},
			want:   tenantMetadata{DisplayName: types.StringValue("a"), Description: types.StringValue("b"), Tags: tagsOf("team", "ml")},
		},
		"a resource with no tags reads them as null": {
			prior:  tenantMetadata{DisplayName: nullString, Description: nullString},
			answer: metadataAnswer{DisplayName: ptr("a"), Description: ptr("")},
			want:   tenantMetadata{DisplayName: types.StringValue("a"), Description: nullString, Tags: nullTags},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := tc.prior.read(tc.answer)
			if !got.DisplayName.Equal(tc.want.DisplayName) || !got.Description.Equal(tc.want.Description) ||
				!got.Tags.Equal(tc.want.Tags) {
				t.Errorf("read = %v %v %v, want %v %v %v", got.DisplayName, got.Description, got.Tags,
					tc.want.DisplayName, tc.want.Description, tc.want.Tags)
			}
		})
	}
}

func fieldsString(f metadataFields) string {
	show := func(p any) string {
		v := reflect.ValueOf(p)
		if v.IsNil() {
			return "<nil>"
		}
		return fmt.Sprintf("%q", v.Elem().Interface())
	}
	return "{" + show(f.DisplayName) + " " + show(f.Description) + " " + show(f.Tags) + "}"
}

// A configuration that wrote "" has not been contradicted by an answer that
// leaves the field out, which a PATCH sending "" can come back with, or by one
// that answers "". Only a real value replaces it.
func TestAnEmptyStringSurvivesAnAnswerThatLeavesItOut(t *testing.T) {
	empty, named := "", "n"
	for _, tc := range []struct {
		name   string
		answer *string
		prior  types.String
		want   types.String
	}{
		{"left out, prior empty", nil, types.StringValue(""), types.StringValue("")},
		{"empty, prior empty", &empty, types.StringValue(""), types.StringValue("")},
		{"left out, prior null", nil, types.StringNull(), types.StringNull()},
		{"empty, prior null", &empty, types.StringNull(), types.StringNull()},
		{"a value, prior empty", &named, types.StringValue(""), types.StringValue("n")},
		{"left out, prior a value", nil, types.StringValue("n"), types.StringNull()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := answeredString(tc.answer, tc.prior); !got.Equal(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
