package provider

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// A cluster list can name an interrupted create by all three parts of its
// scope plus its durable tag. Filtering on only the tag cannot accidentally
// adopt a same-named object in the result.
func TestAClusterListMatchesIDScopeAndCreateToken(t *testing.T) {
	t.Parallel()
	api := listAPI(t, map[string]http.HandlerFunc{
		"GET " + projectPath + "/compute/clusters": paged(t, "clusters", false,
			map[string]any{"id": "c1", "tags": map[string]any{createTokenTag(): "token-1"}},
			map[string]any{"id": "c2", "tags": map[string]any{createTokenTag(): "token-2"}}),
	})
	for _, tc := range []struct {
		name string
		set  map[string]any
		want []string
	}{
		{"id alone", map[string]any{"id": "c2"}, []string{"c2"}},
		{"exact token", map[string]any{"id": "c2", "create_token": "token-2"}, []string{"c2"}},
		{"different token", map[string]any{"id": "c2", "create_token": "token-1"}, []string{}},
		{"token for another object", map[string]any{"create_token": "token-2"}, []string{"c2"}},
		{"no filters", nil, []string{"c1", "c2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, diags := runList(t, api.URL, clusterResourceSuffix, tc.set, false, 100)
			if errs := diagnosticErrors(diags); len(errs) > 0 {
				t.Fatalf("list failed: %v", errs)
			}
			var ids []string
			for _, result := range got {
				ids = append(ids, result.identity["id"])
			}
			if !slices.Equal(ids, tc.want) {
				t.Errorf("found %v with config %v, want %v", ids, tc.set, tc.want)
			}
		})
	}
}

func TestClusterTokenFilterDocumentation(t *testing.T) {
	// Keep the pair rule visible in the public schema. The controller pattern
	// depends on it rather than treating an empty token-only result as safe.
	spec := clusterListSpec()
	got := spec.attributes["create_token"].GetMarkdownDescription()
	for _, want := range []string{"Run the same query with only `id`", "pair this with `id`"} {
		if !strings.Contains(got, want) {
			t.Errorf("create_token description does not say %q:\n%s", want, got)
		}
	}
}
