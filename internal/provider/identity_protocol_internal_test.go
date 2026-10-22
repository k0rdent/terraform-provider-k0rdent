package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A row written before identity existed, and before the scope was recorded
// beside the id, has no identity and a null region and project. The resource
// follows the provider's scope then, and the refresh has to give it an
// identity in that scope: the framework refuses a read that returns none.
// This goes through the protocol because that refusal lives in the framework,
// not in Read.
func TestAPreIdentityRowWithNoScopeStillRefreshes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/regions/global/auth/token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"access_token": "t", "token_type": "Bearer", "expires_in": 900})
	})
	mux.HandleFunc("GET /v1/regions/us-texas-1/projects/demo/networking/security-groups/office", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"id": "office", "state": "active", "ingress": []any{}, "egress": []any{}})
	})
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)

	p := &aiProvider{version: "test", lookupEnv: envMap(saEnv(nil))}
	server, err := providerserver.NewProtocol6WithError(p)()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}

	config := nullObjectWith(t, schemas.Provider.ValueType(), map[string]string{
		"endpoint": api.URL, "region": "us-texas-1", "project": "demo",
	})
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: config})
	if err != nil || len(diagnosticErrors(configured.Diagnostics)) > 0 {
		t.Fatalf("configure: %v %v", err, diagnosticErrors(configured.Diagnostics))
	}

	typeName := ResourceName(securityGroupResourceSuffix)
	state := nullObjectWith(t, schemas.ResourceSchemas[typeName].ValueType(), map[string]string{"id": "office"})
	read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{TypeName: typeName, CurrentState: state})
	if err != nil {
		t.Fatal(err)
	}
	if errs := diagnosticErrors(read.Diagnostics); len(errs) > 0 {
		t.Fatalf("the refresh failed: %v", errs)
	}
	if read.NewIdentity == nil || read.NewIdentity.IdentityData == nil {
		t.Fatal("the refresh returned no identity")
	}
	got, err := read.NewIdentity.IdentityData.Unmarshal(identityType(t, server, typeName))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]tftypes.Value
	if err := got.As(&fields); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"region": "us-texas-1", "project": "demo", "id": "office"} {
		var v string
		if err := fields[name].As(&v); err != nil || v != want {
			t.Errorf("identity %s = %q, want %q", name, v, want)
		}
	}
}

func identityType(t *testing.T, server tfprotov6.ProviderServer, typeName string) tftypes.Type {
	t.Helper()
	resp, err := server.GetResourceIdentitySchemas(context.Background(), &tfprotov6.GetResourceIdentitySchemasRequest{})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := resp.IdentitySchemas[typeName]
	if !ok {
		t.Fatalf("%s declares no identity", typeName)
	}
	return s.ValueType()
}

// nullObjectWith is an object of the given type with every attribute null but
// the named strings.
func nullObjectWith(t *testing.T, typ tftypes.Type, strings map[string]string) *tfprotov6.DynamicValue {
	t.Helper()
	obj, ok := typ.(tftypes.Object)
	if !ok {
		t.Fatalf("%s isn't an object", typ)
	}
	values := map[string]tftypes.Value{}
	for name, at := range obj.AttributeTypes {
		if s, ok := strings[name]; ok {
			values[name] = tftypes.NewValue(at, s)
			continue
		}
		values[name] = tftypes.NewValue(at, nil)
	}
	dv, err := tfprotov6.NewDynamicValue(obj, tftypes.NewValue(obj, values))
	if err != nil {
		t.Fatal(err)
	}
	return &dv
}
