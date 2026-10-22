package provider_test

import (
	"context"
	"strings"
	"testing"

	fwdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

const testVersion = "test"

func newProvider(t *testing.T) fwprovider.Provider {
	t.Helper()
	return provider.New(testVersion)()
}

// Metadata has to report the same type name the resources are prefixed with.
// Nothing else notices when it doesn't: the unit tests and an acceptance run
// both pass with a provider that calls itself something else. Comparing the
// value can't tell a literal from the constant, and that isn't what this is for.
func TestMetadataReportsTheTypeNameAndVersion(t *testing.T) {
	t.Parallel()

	var resp fwprovider.MetadataResponse
	newProvider(t).Metadata(context.Background(), fwprovider.MetadataRequest{}, &resp)

	if resp.TypeName != provider.ProviderTypeName {
		t.Fatalf("TypeName = %q, want the ProviderTypeName constant %q", resp.TypeName, provider.ProviderTypeName)
	}
	if resp.Version != testVersion {
		t.Fatalf("Version = %q, want %q", resp.Version, testVersion)
	}
}

func TestSchemaIsValid(t *testing.T) {
	t.Parallel()

	var resp fwprovider.SchemaResponse
	newProvider(t).Schema(context.Background(), fwprovider.SchemaRequest{}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Schema produced errors: %v", resp.Diagnostics.Errors())
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags.Errors())
	}
}

// Every attribute is optional, because each one has an environment-variable
// fallback and the two that do not have a default are optional by design.
// A required attribute here would break `terraform plan` for anyone
// configuring the provider entirely from the environment, which is the
// supported pipeline shape.
func TestEveryAttributeIsOptional(t *testing.T) {
	t.Parallel()

	var resp fwprovider.SchemaResponse
	newProvider(t).Schema(context.Background(), fwprovider.SchemaRequest{}, &resp)

	for name, attr := range resp.Schema.Attributes {
		if attr.IsRequired() {
			t.Errorf("attribute %q is required; every attribute has an environment fallback", name)
		}
		if !attr.IsOptional() {
			t.Errorf("attribute %q is neither required nor optional, so it cannot be set", name)
		}
		if attr.GetMarkdownDescription() == "" {
			t.Errorf("attribute %q has no description; it would ship to the registry undocumented", name)
		}
	}
}

// The provider block's schema is public surface, so it is pinned.
func TestSchemaExposesExactlyTheTargetingAttributes(t *testing.T) {
	t.Parallel()

	var resp fwprovider.SchemaResponse
	newProvider(t).Schema(context.Background(), fwprovider.SchemaRequest{}, &resp)

	want := map[string]bool{
		"endpoint": true, "region": true, "project": true,
		"operation_timeout": true, "poll_interval": true, "default_tags": true,
	}
	for name := range resp.Schema.Attributes {
		if !want[name] {
			t.Errorf("unexpected provider attribute %q", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("missing provider attribute %q", name)
	}

	if len(resp.Schema.Blocks) != 0 {
		t.Errorf("the provider schema declares %d nested blocks, want none", len(resp.Schema.Blocks))
	}
}

// No credential may be settable through configuration. `Sensitive: true`
// hides a value from output; it does not keep it out of a saved plan. The only
// way a secret cannot reach disk through Terraform is for Terraform never to
// receive it, which means no attribute — not a sensitive one.
func TestNoAttributeCanCarryACredential(t *testing.T) {
	t.Parallel()

	var resp fwprovider.SchemaResponse
	newProvider(t).Schema(context.Background(), fwprovider.SchemaRequest{}, &resp)

	forbidden := []string{
		"token", "api_key", "apikey", "client_secret", "client_id", "secret",
		"password", "credential", "credentials", "credentials_file", "auth",
	}
	for _, name := range forbidden {
		if _, ok := resp.Schema.Attributes[name]; ok {
			t.Errorf("the provider schema declares %q; credentials are read from the environment only", name)
		}
	}

	for name, attr := range resp.Schema.Attributes {
		if attr.IsSensitive() {
			t.Errorf("attribute %q is marked sensitive, which implies it carries a secret; "+
				"secrets must not be schema attributes at all", name)
		}
	}
}

// Organization is not a caller-supplied value anywhere in this API. The
// gateway resolves it from the membership tree in the validated token and
// strips any `x-k0r-*` header a client sets, so an `organization` attribute
// would accept a value and change nothing.
func TestThereIsNoOrganizationAttribute(t *testing.T) {
	t.Parallel()

	var resp fwprovider.SchemaResponse
	newProvider(t).Schema(context.Background(), fwprovider.SchemaRequest{}, &resp)

	for _, name := range []string{"organization", "org", "org_id", "organization_id", "tenant"} {
		if _, ok := resp.Schema.Attributes[name]; ok {
			t.Errorf("the provider schema declares %q; the caller never sends an organization, "+
				"so the attribute would be inert", name)
		}
	}
}

// Every registered name is non-empty, starts with the provider's prefix, and
// is registered once per kind.
//
// It asks the provider for its real registrations and instantiates each one,
// which is the only way to see the name a resource reports. Metadata is a
// method, not a field. Whether a name is built from the constant or spelled
// out can't be seen from the value, and `make name-audit` is what fails on Go
// that writes the name down.
func TestEveryRegisteredNameIsPrefixedAndUnique(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	p := newProvider(t)
	prefix := provider.ResourcePrefix()

	// One set per kind, not one shared set. Resources and data sources are
	// separate namespaces in Terraform, and a managed resource alongside a
	// read-only form of the same name is the convention rather than a
	// collision: the cluster is registered as both, and a configuration says
	// which it means by writing `resource` or `data`. What this check is for
	// is the same thing registered twice in one list, which is a real mistake
	// and still caught.
	resourceNames := map[string]bool{}
	dataSourceNames := map[string]bool{}

	resources := p.Resources(ctx)
	if len(resources) == 0 {
		t.Fatal("the provider registers no resources; it should register at least kubernetes_cluster and instance_group")
	}
	for i, factory := range resources {
		var resp fwresource.MetadataResponse
		factory().Metadata(ctx, fwresource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &resp)
		assertPrefixedUniqueName(t, "resource", i, resp.TypeName, prefix, resourceNames)
	}

	dataSources := p.DataSources(ctx)
	if len(dataSources) == 0 {
		t.Fatal("the provider registers no data sources; the cluster-type catalog is how a user finds a cluster type")
	}
	for i, factory := range dataSources {
		var resp fwdatasource.MetadataResponse
		factory().Metadata(ctx, fwdatasource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &resp)
		assertPrefixedUniqueName(t, "data source", i, resp.TypeName, prefix, dataSourceNames)
	}
}

func assertPrefixedUniqueName(t *testing.T, kind string, index int, name, prefix string, seen map[string]bool) {
	t.Helper()

	if name == "" {
		t.Errorf("%s %d reports no type name", kind, index)
		return
	}
	if !strings.HasPrefix(name, prefix) {
		t.Errorf("%s %q does not start with the provider's prefix %q", kind, name, prefix)
		return
	}
	if seen[name] {
		t.Errorf("%s %q is registered twice", kind, name)
	}
	seen[name] = true
}

// Every registered schema must satisfy the framework's own validation, which
// catches the mistakes that otherwise surface as an unhelpful crash inside
// Terraform rather than as a test failure here.
func TestEveryRegisteredSchemaIsValid(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	p := newProvider(t)

	for _, factory := range p.Resources(ctx) {
		var resp fwresource.SchemaResponse
		r := factory()
		r.Schema(ctx, fwresource.SchemaRequest{}, &resp)
		var meta fwresource.MetadataResponse
		r.Metadata(ctx, fwresource.MetadataRequest{}, &meta)
		if resp.Diagnostics.HasError() {
			t.Errorf("resource %q schema produced errors: %v", meta.TypeName, resp.Diagnostics.Errors())
			continue
		}
		if diags := resp.Schema.ValidateImplementation(ctx); diags.HasError() {
			t.Errorf("resource %q failed framework validation: %v", meta.TypeName, diags.Errors())
		}
	}

	for _, factory := range p.DataSources(ctx) {
		var resp fwdatasource.SchemaResponse
		d := factory()
		d.Schema(ctx, fwdatasource.SchemaRequest{}, &resp)
		var meta fwdatasource.MetadataResponse
		d.Metadata(ctx, fwdatasource.MetadataRequest{}, &meta)
		if resp.Diagnostics.HasError() {
			t.Errorf("data source %q schema produced errors: %v", meta.TypeName, resp.Diagnostics.Errors())
			continue
		}
		if diags := resp.Schema.ValidateImplementation(ctx); diags.HasError() {
			t.Errorf("data source %q failed framework validation: %v", meta.TypeName, diags.Errors())
		}
	}
}
