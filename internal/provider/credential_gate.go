package provider

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/list"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// The schema credential gate.
//
// No secret may reach a state file or a saved plan, and the schema is where
// that's honoured or quietly broken. Everything a provider block, a resource
// or a data source holds is written to one or the other, and `Sensitive: true`
// only hides a value from the console. So on those surfaces a credential has
// no attribute at all, at any depth, and an attribute marked Sensitive is
// refused too, because marking it says it carries one. An ephemeral resource
// is the one surface that never persists, so a credential belongs there, but
// only as a computed-only attribute marked Sensitive. Computed-only, because
// one a configuration can set is a way for a secret to arrive through a `.tf`
// file, and that file is checked in. Sensitive, so it stays off the console as
// well. The kubeconfig's `token` and `client_key` are such attributes: they are
// the cluster's credential, minted for the caller and handed on to a
// kubernetes provider, not the provider's own.
//
// A write-only attribute is the one way a credential can sit on a surface
// that persists, because Terraform never writes it there: it reaches the
// provider during the plan and apply and is null in state and in a saved
// plan. That's how a resource takes a secret it has to send but mustn't keep,
// a webhook subscription's signing secret for one. It's configurable, but the
// worry about a secret in a checked-in `.tf` file is met another way:
// Terraform takes a write-only value from an ephemeral resource or an
// ephemeral variable, which is where the attribute's documentation points. So
// it passes, named like a credential or marked Sensitive, as long as nothing
// computes it: a computed value is one Terraform stores. Terraform before 1.11
// refuses to set a write-only attribute at all.
//
// The rule is a name pattern rather than a list of exact names, because the
// names a secret arrives under are open-ended (`access_token`, `db_password`,
// `ssh_private_key`), and a list only catches the ones somebody thought of.
// The walk reaches every nested attribute and every block, since a nested
// object is where an upstream struct gets mapped wholesale. It reaches the
// field names inside an attribute's type too: an ObjectAttribute, or a list,
// set or map of objects, declares its fields in the type rather than as
// attributes, and a field there is written to state the same way.

// credentialFragments are what a credential's name contains, matched with the
// separators taken out so `api_key`, `apiKey` and `apikey` are one rule. Each
// one is specific enough that no attribute in this provider matches it by
// accident: a bare `key` would catch `pkey`, the InfiniBand partition key, and
// `ssh_public_keys`, neither of which is secret.
var credentialFragments = []string{
	"token", "secret", "passw", "passphrase", "credential", "apikey", "privatekey",
	"clientkey", "accesskey", "signingkey", "sessionkey", "kubeconfig", "bearer", "cookie", "jwt",
}

// credentialWords are matched against whole words, because as fragments they
// are inside ordinary names: `auth` is in `author`.
var credentialWords = []string{"auth", "authn", "authz", "authorization", "authentication"}

// describingWords, as the last word of a name, make it something said about a
// credential rather than the credential: `secret_name`, `credential_ref`,
// `kubeconfig_id`, `cookie_domain`, `password_policy`, `token_ttl`. An OAuth
// `client_id` is one too: it's public, and sent in the clear beside the secret
// it goes with. `file` and `path` aren't here, because `credentials_file` is
// where a credential is kept, and the gate has always refused it.
var describingWords = []string{
	"id", "ids", "name", "names", "ref", "refs", "reference", "domain", "policy",
	"type", "kind", "version", "ttl", "expiry", "expiration", "at", "count", "length",
}

// countingWords anywhere in a name make it a number about credentials, not
// one: `max_tokens`, `tokens_per_second`.
var countingWords = []string{"max", "min", "num", "count", "limit", "per", "quota"}

// LooksLikeCredential reports whether an attribute's name says it holds a
// credential. A credential word in the name isn't enough: the name also mustn't
// end in a word that makes it a description of one, or count one.
func LooksLikeCredential(name string) bool {
	words := nameWords(name)
	if len(words) > 1 && slices.Contains(describingWords, words[len(words)-1]) {
		return false
	}
	if slices.ContainsFunc(words, func(w string) bool { return slices.Contains(countingWords, w) }) {
		return false
	}
	joined := strings.Join(words, "")
	for _, f := range credentialFragments {
		if strings.Contains(joined, f) {
			return true
		}
	}
	return slices.ContainsFunc(words, func(w string) bool { return slices.Contains(credentialWords, w) })
}

// nameWords splits a name into its lower-case words, at `_`, `-` and a
// lower-case letter or digit followed by an upper-case one, so `secret_name`
// and `secretName` are the same two words.
func nameWords(name string) []string {
	var words []string
	var cur strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		switch {
		case r == '_' || r == '-':
			if cur.Len() > 0 {
				words = append(words, cur.String())
				cur.Reset()
			}
			continue
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])):
			if cur.Len() > 0 {
				words = append(words, cur.String())
				cur.Reset()
			}
		}
		cur.WriteRune(unicode.ToLower(r))
	}
	if cur.Len() > 0 {
		words = append(words, cur.String())
	}
	return words
}

// SchemaSurface is where a schema's values end up, which decides what it may
// carry.
type SchemaSurface int

const (
	// PersistedSurface is a provider block, a resource, a data source or an
	// action, whose values reach state or a saved plan. An action's
	// configuration is planned, so a saved plan carries it. A list resource's
	// configuration is held to it too. It sits in a `.tfquery.hcl` file that's
	// checked in beside the rest, so it's no place for a credential either.
	PersistedSurface SchemaSurface = iota
	// EphemeralSurface is an ephemeral resource, whose values never persist.
	EphemeralSurface
)

// CredentialViolations walks one schema, every nested attribute, block and
// field of an attribute's type included, and returns each attribute that breaks the gate, by dotted path.
// schema is any of the framework's Schema types: the walk reads them through
// the methods they share, since the framework keeps the interface behind
// those methods internal.
func CredentialViolations(surface SchemaSurface, schema any) []string {
	var out []string
	walkSchemaAttributes(reflect.ValueOf(schema), "", func(path string, a attributeFlags) {
		name := path[strings.LastIndex(path, ".")+1:]
		credential := LooksLikeCredential(name)
		switch {
		case surface == PersistedSurface && a.writeOnly && !a.computed:
			// Never written to state or a plan: see the comment at the top.
		case surface == PersistedSurface && credential:
			out = append(out, fmt.Sprintf("%s is named like a credential, and this surface is written to state or a plan", path))
		case surface == PersistedSurface && a.sensitive && !a.field:
			out = append(out, fmt.Sprintf("%s is marked Sensitive, which says it carries a secret, and this surface is written to state or a plan", path))
		case surface == EphemeralSurface && credential && a.configurable:
			out = append(out, fmt.Sprintf("%s is named like a credential and a configuration can set it", path))
		case surface == EphemeralSurface && credential && !a.sensitive:
			out = append(out, fmt.Sprintf("%s is named like a credential and isn't marked Sensitive", path))
		}
	})
	slices.Sort(out)
	return out
}

// attributeFlags are the parts of an attribute's declaration the gate reads.
// configurable is Optional or Required, which is what lets a `.tf` file set it.
// field marks a name read from inside an attribute's type, which carries the
// flags of the attribute that declares it. A Sensitive one is reported once,
// at the attribute, rather than again for each field.
type attributeFlags struct {
	sensitive, configurable, computed, field, writeOnly bool
}

// walkSchemaAttributes calls fn for every attribute under v, which is a schema
// or a nested object, descending into nested attributes and blocks.
func walkSchemaAttributes(v reflect.Value, prefix string, fn func(path string, a attributeFlags)) {
	for _, method := range []string{"GetAttributes", "GetBlocks"} {
		get := v.MethodByName(method)
		if !get.IsValid() {
			continue
		}
		children := get.Call(nil)[0]
		keys := children.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		for _, k := range keys {
			child := children.MapIndex(k).Elem()
			path := prefix + k.String()
			a, ok := child.Interface().(interface {
				IsSensitive() bool
				IsOptional() bool
				IsRequired() bool
				IsComputed() bool
				GetType() attr.Type
			})
			if ok {
				flags := attributeFlags{
					sensitive: a.IsSensitive(), configurable: a.IsOptional() || a.IsRequired(), computed: a.IsComputed(),
				}
				// Only a managed resource's attributes can be write-only, so
				// it's asked for rather than required.
				if wo, ok := child.Interface().(interface{ IsWriteOnly() bool }); ok {
					flags.writeOnly = wo.IsWriteOnly()
				}
				fn(path, flags)
				if !child.MethodByName("GetNestedObject").IsValid() {
					// A nested attribute's type is its nested object again,
					// which the walk below reaches with each field's own flags.
					flags.field = true
					walkTypeFields(a.GetType(), path+".", flags, fn)
				}
			}
			if nested := child.MethodByName("GetNestedObject"); nested.IsValid() {
				walkSchemaAttributes(nested.Call(nil)[0], path+".", fn)
			}
		}
	}
}

// walkTypeFields calls fn for every field name inside an attribute's type:
// an object's fields, and those of an object a list, set, map or tuple holds,
// at any depth. A collection adds nothing to the path, since a field is named
// the same in every element.
func walkTypeFields(typ attr.Type, prefix string, flags attributeFlags, fn func(path string, a attributeFlags)) {
	switch t := typ.(type) {
	case attr.TypeWithAttributeTypes:
		fields := t.AttributeTypes()
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			fn(prefix+name, flags)
			walkTypeFields(fields[name], prefix+name+".", flags, fn)
		}
	case attr.TypeWithElementType:
		walkTypeFields(t.ElementType(), prefix, flags, fn)
	case attr.TypeWithElementTypes:
		for _, elem := range t.ElementTypes() {
			walkTypeFields(elem, prefix, flags, fn)
		}
	}
}

// SchemaCredentialFindings runs the gate over every schema the provider
// registers: its own block, each resource, data source, ephemeral resource,
// list resource and action.
// Each finding names the surface. surfaces is how many schemas were walked.
func SchemaCredentialFindings(ctx context.Context) (findings []string, surfaces int) {
	p := New("credential-gate")()
	add := func(kind, name string, surface SchemaSurface, schema any) {
		surfaces++
		for _, v := range CredentialViolations(surface, schema) {
			findings = append(findings, fmt.Sprintf("%s %s: %s", kind, name, v))
		}
	}

	var ps fwprovider.SchemaResponse
	p.Schema(ctx, fwprovider.SchemaRequest{}, &ps)
	add("provider", ProviderTypeName, PersistedSurface, ps.Schema)

	for _, newResource := range p.Resources(ctx) {
		r := newResource()
		var md resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		var s resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &s)
		surfaces++
		for _, v := range CredentialViolations(PersistedSurface, s.Schema) {
			findings = append(findings, fmt.Sprintf("resource %s: %s", md.TypeName, v))
		}
	}
	for _, newDataSource := range p.DataSources(ctx) {
		d := newDataSource()
		var md datasource.MetadataResponse
		d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		var s datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &s)
		add("data source", md.TypeName, PersistedSurface, s.Schema)
	}
	if ep, ok := p.(fwprovider.ProviderWithEphemeralResources); ok {
		for _, newEphemeral := range ep.EphemeralResources(ctx) {
			e := newEphemeral()
			var md ephemeral.MetadataResponse
			e.Metadata(ctx, ephemeral.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
			var s ephemeral.SchemaResponse
			e.Schema(ctx, ephemeral.SchemaRequest{}, &s)
			add("ephemeral resource", md.TypeName, EphemeralSurface, s.Schema)
		}
	}
	if lp, ok := p.(fwprovider.ProviderWithListResources); ok {
		for _, newList := range lp.ListResources(ctx) {
			l := newList()
			var md resource.MetadataResponse
			l.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
			var s list.ListResourceSchemaResponse
			l.ListResourceConfigSchema(ctx, list.ListResourceSchemaRequest{}, &s)
			add("list resource", md.TypeName, PersistedSurface, s.Schema)
		}
	}
	if ap, ok := p.(fwprovider.ProviderWithActions); ok {
		for _, newAction := range ap.Actions(ctx) {
			a := newAction()
			var md action.MetadataResponse
			a.Metadata(ctx, action.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
			var s action.SchemaResponse
			a.Schema(ctx, action.SchemaRequest{}, &s)
			add("action", md.TypeName, PersistedSurface, s.Schema)
		}
	}
	return findings, surfaces
}
