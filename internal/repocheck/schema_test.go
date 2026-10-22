package repocheck

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// importGuide is the template the import guide is generated from.
const importGuide = "templates/guides/import.md.tmpl"

// everyUnimportableSurfaceIsNamed asserts that whatever can't be imported is
// said to be so, in the import guide and on its own page. That's every
// ephemeral resource, which has no state to import into, and any managed
// resource without an import. A surface added to either list fails here until
// both places name it, so the gap is written down before anyone finds it.
func everyUnimportableSurfaceIsNamed(guide string, all []surface) error {
	unimportable := map[string]bool{}
	if err := eachResource(nil, "import", func(r resource.Resource, name string) bool {
		if _, ok := r.(resource.ResourceWithImportState); !ok {
			unimportable[name] = true
		}
		return true
	}); err != nil {
		return err
	}
	var missing []string
	for _, s := range all {
		cannotImport := s.Kind == "ephemeral resource" || (s.Kind == "resource" && unimportable[s.Name])
		if !cannotImport {
			continue
		}
		if !strings.Contains(guide, s.Name) {
			missing = append(missing, fmt.Sprintf("%s isn't named in %s", s.Name, importGuide))
		}
		if !strings.Contains(strings.ToLower(s.Doc), "import") {
			missing = append(missing, fmt.Sprintf("%s's own description doesn't say it can't be imported", s.Name))
		}
	}
	if len(missing) > 0 {
		return errors.New(strings.Join(missing, ", "))
	}
	return nil
}

func TestEveryUnimportableSurfaceIsNamed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), importGuide))
	if err != nil {
		t.Fatal(err)
	}
	all := surfaces()
	if err := everyUnimportableSurfaceIsNamed(string(raw), all); err != nil {
		t.Error(err)
	}
	// A guide that names none of them has to fail, or the check reads nothing.
	if err := everyUnimportableSurfaceIsNamed("", all); err == nil {
		t.Error("an import guide naming no unimportable surface passed")
	}
}

// timeoutExceptions are the resources that declare no timeouts block, and why.
var timeoutExceptions = map[string]string{
	"webhook_subscription": "each of its calls answers at once, with nothing to wait for",
}

// everyWaitingResourceHasTimeouts asserts every registered resource, and every
// ephemeral resource, declares a `timeouts` block. Each of them polls the
// platform for something to settle, so each of them waits. A resource that
// genuinely doesn't wait goes in except, with the reason.
func everyWaitingResourceHasTimeouts(except map[string]string) error {
	ctx := context.Background()
	if err := eachResource(except, "have one", func(_ resource.Resource, name string) bool {
		s, _ := resourceSchema(name)
		_, ok := s.Blocks["timeouts"]
		return ok
	}); err != nil {
		return err
	}
	ep, ok := provider.New("test")().(fwprovider.ProviderWithEphemeralResources)
	if !ok {
		return nil
	}
	for _, newEphemeral := range ep.EphemeralResources(ctx) {
		e := newEphemeral()
		var md ephemeral.MetadataResponse
		e.Metadata(ctx, ephemeral.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
		var sr ephemeral.SchemaResponse
		e.Schema(ctx, ephemeral.SchemaRequest{}, &sr)
		if _, ok := sr.Schema.Blocks["timeouts"]; !ok {
			return fmt.Errorf("the ephemeral resource %s declares no timeouts block", md.TypeName)
		}
	}
	return nil
}

func TestEveryWaitingResourceHasTimeouts(t *testing.T) {
	if err := everyWaitingResourceHasTimeouts(timeoutExceptions); err != nil {
		t.Error(err)
	}
	// An exception that has a timeouts block after all is stale.
	stale := map[string]string{"kubernetes_cluster": "x"}
	for k, v := range timeoutExceptions {
		stale[k] = v
	}
	if err := everyWaitingResourceHasTimeouts(stale); err == nil || !strings.Contains(err.Error(), "drop the exception") {
		t.Errorf("a stale exception gave %v", err)
	}
}

// eachResource applies has to every registered resource and fails on the first
// that lacks the property and isn't an exception, or on an exception that has
// the property after all.
func eachResource(except map[string]string, verb string, has func(r resource.Resource, name string) bool) error {
	ctx := context.Background()
	excepted := map[string]bool{}
	for suffix := range except {
		excepted[provider.ResourceName(suffix)] = true
	}
	holding := 0
	for _, newResource := range provider.New("test")().Resources(ctx) {
		r := newResource()
		var md resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
		switch ok := has(r, md.TypeName); {
		case ok && excepted[md.TypeName]:
			return fmt.Errorf("%s is named as an exception but %s now: drop the exception", md.TypeName, verb)
		case !ok && !excepted[md.TypeName]:
			return fmt.Errorf("%s does not %s, and is not named as an exception", md.TypeName, verb)
		case ok:
			holding++
		}
		delete(excepted, md.TypeName)
	}
	if len(excepted) > 0 {
		stale := make([]string, 0, len(excepted))
		for name := range excepted {
			stale = append(stale, name)
		}
		sort.Strings(stale)
		return fmt.Errorf("%s named as an exception but not registered", strings.Join(stale, ", "))
	}
	if holding == 0 {
		return fmt.Errorf("no resource is registered, so this proves nothing")
	}
	return nil
}

// resourceSchema returns a registered resource's schema by type name.
func resourceSchema(name string) (rschema.Schema, bool) {
	ctx := context.Background()
	for _, newResource := range provider.New("test")().Resources(ctx) {
		r := newResource()
		var md resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
		if md.TypeName != name {
			continue
		}
		var sr resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &sr)
		return sr.Schema, true
	}
	return rschema.Schema{}, false
}
