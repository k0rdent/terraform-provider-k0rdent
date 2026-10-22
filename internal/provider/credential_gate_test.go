package provider_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	aschema "github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	eschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	pschema "github.com/hashicorp/terraform-plugin-framework/provider/schema"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The gate over everything the provider registers: its own block, every
// resource, data source, ephemeral resource and action, at every depth.
func TestNoSchemaCanCarryACredential(t *testing.T) {
	findings, surfaces := provider.SchemaCredentialFindings(context.Background())
	for _, f := range findings {
		t.Error(f)
	}
	// One provider block, and at least one of each kind behind it. A walk that
	// found nothing to walk would pass for the wrong reason.
	if surfaces < 4 {
		t.Errorf("the gate walked %d schemas; it should reach the provider block and every registration", surfaces)
	}
}

// The gate walks one schema per registration, the provider block's included,
// so its count is the provider's own. A kind it skipped would be one whose
// inputs a credential could arrive through unchecked.
func TestTheCredentialGateWalksEveryRegistration(t *testing.T) {
	ctx := context.Background()
	p := provider.New("count")()
	want := 1 + len(p.Resources(ctx)) + len(p.DataSources(ctx))
	if ep, ok := p.(fwprovider.ProviderWithEphemeralResources); ok {
		want += len(ep.EphemeralResources(ctx))
	}
	if lp, ok := p.(fwprovider.ProviderWithListResources); ok {
		want += len(lp.ListResources(ctx))
	}
	if ap, ok := p.(fwprovider.ProviderWithActions); ok {
		want += len(ap.Actions(ctx))
	}
	if _, got := provider.SchemaCredentialFindings(ctx); got != want {
		t.Fatalf("the gate walked %d schemas, and the provider registers %d", got, want)
	}
}

// The gate has to be able to fail. Each schema here is one a future change
// could plausibly add, and each has to be refused, at the depth and on the
// surface it is declared.
func TestTheCredentialGateRefusesWhatItShould(t *testing.T) {
	for _, tc := range []struct {
		name    string
		surface provider.SchemaSurface
		schema  any
		want    string
	}{
		{"a resource's nested Sensitive attribute", provider.PersistedSurface, rschema.Schema{Attributes: map[string]rschema.Attribute{
			"spec": rschema.SingleNestedAttribute{Optional: true, Attributes: map[string]rschema.Attribute{
				"value": rschema.StringAttribute{Optional: true, Sensitive: true},
			}},
		}}, "spec.value is marked Sensitive"},
		{"a Sensitive attribute in a list of objects", provider.PersistedSurface, rschema.Schema{Attributes: map[string]rschema.Attribute{
			"members": rschema.ListNestedAttribute{Optional: true, NestedObject: rschema.NestedAttributeObject{
				Attributes: map[string]rschema.Attribute{"note": rschema.StringAttribute{Optional: true, Sensitive: true}},
			}},
		}}, "members.note is marked Sensitive"},
		{"a credential inside a block", provider.PersistedSurface, rschema.Schema{Blocks: map[string]rschema.Block{
			"login": rschema.SingleNestedBlock{Attributes: map[string]rschema.Attribute{
				"db_password": rschema.StringAttribute{Optional: true},
			}},
		}}, "login.db_password is named like a credential"},
		{"a data source's access_token", provider.PersistedSurface, dschema.Schema{Attributes: map[string]dschema.Attribute{
			"access_token": dschema.StringAttribute{Computed: true},
		}}, "access_token is named like a credential"},
		{"an action's api_key, which a saved plan carries", provider.PersistedSurface, aschema.Schema{Attributes: map[string]aschema.Attribute{
			"api_key": aschema.StringAttribute{Optional: true},
		}}, "api_key is named like a credential"},
		{"a provider block's Sensitive attribute", provider.PersistedSurface, pschema.Schema{Attributes: map[string]pschema.Attribute{
			"endpoint": pschema.StringAttribute{Optional: true, Sensitive: true},
		}}, "endpoint is marked Sensitive"},
		{"a provider block's private key, even Sensitive", provider.PersistedSurface, pschema.Schema{Attributes: map[string]pschema.Attribute{
			"ssh_private_key": pschema.StringAttribute{Optional: true, Sensitive: true},
		}}, "ssh_private_key is named like a credential"},
		{"an ephemeral password that isn't Sensitive", provider.EphemeralSurface, eschema.Schema{Attributes: map[string]eschema.Attribute{
			"db_password": eschema.StringAttribute{Computed: true},
		}}, "db_password is named like a credential and isn't marked Sensitive"},
		{"an ephemeral nested token that isn't Sensitive", provider.EphemeralSurface, eschema.Schema{Attributes: map[string]eschema.Attribute{
			"user": eschema.SingleNestedAttribute{Computed: true, Attributes: map[string]eschema.Attribute{
				"bearerToken": eschema.StringAttribute{Computed: true},
			}},
		}}, "user.bearerToken is named like a credential and isn't marked Sensitive"},
		{"a credential in an object attribute's type", provider.PersistedSurface, rschema.Schema{Attributes: map[string]rschema.Attribute{
			"login": rschema.ObjectAttribute{Optional: true, AttributeTypes: map[string]attr.Type{"access_token": types.StringType}},
		}}, "login.access_token is named like a credential"},
		{"a credential in a list of objects' element type", provider.PersistedSurface, rschema.Schema{Attributes: map[string]rschema.Attribute{
			"users": rschema.ListAttribute{Optional: true, ElementType: types.ObjectType{AttrTypes: map[string]attr.Type{"password": types.StringType}}},
		}}, "users.password is named like a credential"},
		{"a credential deep in a map of sets of objects", provider.PersistedSurface, dschema.Schema{Attributes: map[string]dschema.Attribute{
			"pools": dschema.MapAttribute{Computed: true, ElementType: types.SetType{ElemType: types.ObjectType{AttrTypes: map[string]attr.Type{
				"spec": types.ObjectType{AttrTypes: map[string]attr.Type{"client_secret": types.StringType}},
			}}}},
		}}, "pools.spec.client_secret is named like a credential"},
		{"a credential in an object attribute inside a nested attribute", provider.PersistedSurface, rschema.Schema{Attributes: map[string]rschema.Attribute{
			"spec": rschema.SingleNestedAttribute{Optional: true, Attributes: map[string]rschema.Attribute{
				"settings": rschema.ObjectAttribute{Optional: true, AttributeTypes: map[string]attr.Type{"token": types.StringType}},
			}},
		}}, "spec.settings.token is named like a credential"},
		{"an ephemeral object attribute's token that isn't Sensitive", provider.EphemeralSurface, eschema.Schema{Attributes: map[string]eschema.Attribute{
			"session": eschema.ObjectAttribute{Computed: true, AttributeTypes: map[string]attr.Type{"token": types.StringType}},
		}}, "session.token is named like a credential and isn't marked Sensitive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := provider.CredentialViolations(tc.surface, tc.schema)
			found := false
			for _, v := range got {
				found = found || strings.HasPrefix(v, tc.want)
			}
			if !found {
				t.Errorf("violations = %q, want one starting %q", got, tc.want)
			}
		})
	}
}

// And it lets through what it should, or it would be routed around. A
// credential on an ephemeral resource, marked Sensitive, is where one belongs,
// and these names are ordinary ones that happen to sit near a credential word.
func TestTheCredentialGateLetsOrdinaryAttributesThrough(t *testing.T) {
	ephemeral := eschema.Schema{Attributes: map[string]eschema.Attribute{
		"kubeconfig":   eschema.StringAttribute{Computed: true, Sensitive: true},
		"access_token": eschema.StringAttribute{Computed: true, Sensitive: true},
		"raw":          eschema.StringAttribute{Computed: true, Sensitive: true},
		"session": eschema.ObjectAttribute{Computed: true, Sensitive: true, AttributeTypes: map[string]attr.Type{
			"token": types.StringType,
		}},
	}}
	if got := provider.CredentialViolations(provider.EphemeralSurface, ephemeral); len(got) > 0 {
		t.Errorf("an ephemeral resource's Sensitive credentials were refused: %q", got)
	}

	persisted := rschema.Schema{Attributes: map[string]rschema.Attribute{
		"pkey":               rschema.StringAttribute{Optional: true},
		"ssh_public_keys":    rschema.ListAttribute{Optional: true, ElementType: types.StringType},
		"kubernetes_version": rschema.StringAttribute{Computed: true},
		"author":             rschema.StringAttribute{Optional: true},
		"key_name":           rschema.StringAttribute{Optional: true},
	}}
	if got := provider.CredentialViolations(provider.PersistedSurface, persisted); len(got) > 0 {
		t.Errorf("ordinary attributes were refused: %q", got)
	}
}

// The names the gate matched exactly before it matched a pattern are all
// still refused, apart from `client_id`. An OAuth client id is public, sent
// in the clear beside the secret it goes with, and refusing it would push a
// real attribute into a misleading name.
func TestEveryNameTheOldGateListedStillLooksLikeACredential(t *testing.T) {
	for _, name := range []string{
		"token", "api_key", "apikey", "client_secret", "secret",
		"password", "credential", "credentials", "credentials_file", "auth",
	} {
		if !provider.LooksLikeCredential(name) {
			t.Errorf("%q no longer looks like a credential", name)
		}
	}
}

// A name is a credential when it says it holds one, not when a credential
// word is somewhere in it. A count of tokens, the name of a secret, a
// reference to a credential, a cookie's domain and a password's policy are
// all safe to keep in state, and refusing them would only push them into
// names that say less.
func TestCredentialNamesAreTheOnesThatHoldOne(t *testing.T) {
	for name, want := range map[string]bool{
		// Each holds the secret itself.
		"token": true, "access_token": true, "refresh_token": true, "bearerToken": true,
		"client_secret": true, "secret": true, "webhook_secret": true,
		"password": true, "db_password": true, "passphrase": true,
		"api_key": true, "apiKey": true, "private_key": true, "ssh_private_key": true,
		"client_key": true, "clientKey": true,
		"credential": true, "credentials": true, "credentials_file": true,
		"kubeconfig": true, "kubeconfig_raw": true, "raw_kubeconfig": true,
		"session_cookie": true, "auth": true, "authorization": true,

		// Each says something about a secret, or counts tokens, and holds none.
		"max_tokens": false, "tokens_per_second": false, "token_count": false,
		"secret_name": false, "secret_ref": false, "credential_ref": false,
		"kubeconfig_id": false, "client_id": false, "cookie_domain": false,
		"password_policy": false, "token_ttl": false, "secretName": false,
		"key_name": false, "pkey": false, "ssh_public_keys": false, "author": false,
	} {
		if got := provider.LooksLikeCredential(name); got != want {
			t.Errorf("LooksLikeCredential(%q) = %t, want %t", name, got, want)
		}
	}
}

// A credential on an ephemeral resource passes only when nothing in a `.tf`
// file can set it and it's marked Sensitive. Computed-only, Terraform never
// writes it to state or a plan, and the marking keeps it off the console, so
// the kubeconfig's `token` passes. Unmarked, it would print, and configurable,
// it's a way in for a secret, so either one is refused, as the same name is
// on every surface that persists, whatever its flags.
func TestOnlyAComputedSensitiveEphemeralCredentialPasses(t *testing.T) {
	t.Parallel()
	type flags struct{ optional, required, computed, sensitive bool }
	computed := flags{computed: true, sensitive: true}
	schemaFor := func(kind, name string, f flags) (provider.SchemaSurface, any) {
		switch kind {
		case "ephemeral resource":
			return provider.EphemeralSurface, eschema.Schema{Attributes: map[string]eschema.Attribute{
				name: eschema.StringAttribute{Optional: f.optional, Required: f.required, Computed: f.computed, Sensitive: f.sensitive},
			}}
		case "data source":
			return provider.PersistedSurface, dschema.Schema{Attributes: map[string]dschema.Attribute{
				name: dschema.StringAttribute{Optional: f.optional, Required: f.required, Computed: f.computed, Sensitive: f.sensitive},
			}}
		case "resource":
			return provider.PersistedSurface, rschema.Schema{Attributes: map[string]rschema.Attribute{
				name: rschema.StringAttribute{Optional: f.optional, Required: f.required, Computed: f.computed, Sensitive: f.sensitive},
			}}
		default:
			return provider.PersistedSurface, pschema.Schema{Attributes: map[string]pschema.Attribute{
				name: pschema.StringAttribute{Optional: f.optional, Required: f.required, Sensitive: f.sensitive},
			}}
		}
	}
	cases := []struct {
		kind string
		name string
		f    flags
		want bool
	}{
		{"ephemeral resource", "token", computed, false},
		// The kubeconfig's other credential. The refused cases make sure the
		// pass above is the gate passing it, not the name slipping past.
		{"ephemeral resource", "client_key", computed, false},
		{"ephemeral resource", "client_key", flags{computed: true}, true},
		{"data source", "client_key", flags{computed: true}, true},
		{"ephemeral resource", "token", flags{computed: true}, true},
		{"ephemeral resource", "foo_token", flags{computed: true}, true},
		{"ephemeral resource", "token", flags{optional: true}, true},
		{"ephemeral resource", "token", flags{optional: true, sensitive: true}, true},
		{"ephemeral resource", "token", flags{required: true, sensitive: true}, true},
		{"ephemeral resource", "token", flags{optional: true, computed: true}, true},
		{"ephemeral resource", "token", flags{optional: true, computed: true, sensitive: true}, true},
		{"data source", "token", computed, true},
		{"resource", "token", computed, true},
		{"provider", "token", flags{optional: true}, true},
		{"resource", "host", flags{optional: true}, false},
	}
	for _, tc := range cases {
		surface, schema := schemaFor(tc.kind, tc.name, tc.f)
		got := provider.CredentialViolations(surface, schema)
		if refused := len(got) > 0; refused != tc.want {
			t.Errorf("%s %s %+v: refused = %t, want %t (%q)", tc.kind, tc.name, tc.f, refused, tc.want, got)
		}
	}

	// The same rule at depth: a nested credential an ephemeral resource lets a
	// configuration set is refused, even marked Sensitive.
	nested := eschema.Schema{Attributes: map[string]eschema.Attribute{
		"auth": eschema.SingleNestedAttribute{Optional: true, Sensitive: true, Attributes: map[string]eschema.Attribute{
			"token": eschema.StringAttribute{Optional: true, Sensitive: true},
		}},
	}}
	got := provider.CredentialViolations(provider.EphemeralSurface, nested)
	if !slices.ContainsFunc(got, func(v string) bool {
		return strings.HasPrefix(v, "auth.token is named like a credential and a configuration can set it")
	}) {
		t.Errorf("a configurable nested token on an ephemeral resource passed: %q", got)
	}
}

// A write-only attribute is the one place a resource can take a credential,
// because Terraform never writes its value to state or a plan. The same name
// as an ordinary attribute, configurable or computed, marked or not, is still
// refused.
func TestOnlyAWriteOnlyCredentialPassesOnAResource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		attr rschema.Attribute
		want bool
	}{
		{rschema.StringAttribute{Optional: true, WriteOnly: true}, false},
		{rschema.StringAttribute{Required: true, WriteOnly: true}, false},
		{rschema.StringAttribute{Optional: true, WriteOnly: true, Sensitive: true}, false},
		// Write-only doesn't widen to an attribute Terraform also computes and
		// stores. The framework refuses that schema too, and the gate doesn't
		// lean on it.
		{rschema.StringAttribute{Optional: true, Computed: true, WriteOnly: true}, true},
		{rschema.StringAttribute{Optional: true}, true},
		{rschema.StringAttribute{Optional: true, Sensitive: true}, true},
		{rschema.StringAttribute{Optional: true, Computed: true, Sensitive: true}, true},
		{rschema.StringAttribute{Computed: true, Sensitive: true}, true},
		// The sealed-secret exception's shape, computed only and unmarked.
		// The write-only rule doesn't take it: only that exception does, on
		// its one resource, through ResourceCredentialViolations.
		{rschema.StringAttribute{Computed: true}, true},
	} {
		schema := rschema.Schema{Attributes: map[string]rschema.Attribute{"secret_wo": tc.attr}}
		got := provider.CredentialViolations(provider.PersistedSurface, schema)
		if refused := len(got) > 0; refused != tc.want {
			t.Errorf("%+v: refused = %t, want %t (%q)", tc.attr, refused, tc.want, got)
		}
	}
}
