package provider

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/list"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// The deprecation policy: a deprecated resource or attribute warns for a
// published period before removal.
//
// The warning is the framework's DeprecationMessage, which Terraform shows on
// every validate and plan whose configuration uses the deprecated thing. The
// period is the one docs/public-compatibility.md publishes, the AWS and
// Google providers' practice: a deprecation keeps working, and warning, until
// the next major version, and only a major removes it.
//
// So a message has to name that major, exactly. "Will be removed in a future
// version" warns without saying how long a reader has. A later major promises
// more than the policy keeps, and the current one or an earlier one promises
// a removal the policy doesn't allow. Nothing is deprecated today, so this
// holds the first deprecation to it rather than any existing one.

// removalVersion is the part of a message that names the release the
// deprecated thing goes in: "removed in v1.0.0". The major is captured.
var removalVersion = regexp.MustCompile(`(?i)\bremoved in v([0-9]+)\.0\.0\b`)

// releasedHeading matches a dated release heading in CHANGELOG.md in either
// style the file uses, Keep a Changelog's "## [0.3.0] - 2026-10-20" and
// HashiCorp's "## 1.0.0 (November 3, 2026)", and captures the major.
var releasedHeading = regexp.MustCompile(`(?m)^## (?:\[([0-9]+)\.[0-9]+\.[0-9]+[^\]]*\][^\n]*[0-9]{4}-[0-9]{2}-[0-9]{2}|` +
	`([0-9]+)\.[0-9]+\.[0-9]+\S* \([A-Z][a-z]+ [0-9]{1,2}, [0-9]{4}\))`)

// ReleasedMajor is the major version of the latest release CHANGELOG.md
// records, or 0 when it records none. A deprecation made now is removed in
// the major after it.
//
// The changelog rather than the binary's version, because the binary is
// "dev" everywhere but a release build, and the release pull request is what
// writes the dated heading, so this moves on the day a major ships.
func ReleasedMajor(changelog []byte) int {
	major := 0
	for _, m := range releasedHeading.FindAllSubmatch(changelog, -1) {
		for _, g := range m[1:] {
			if n, err := strconv.Atoi(string(g)); err == nil && n > major {
				major = n
			}
		}
	}
	return major
}

// Deprecations counts every DeprecationMessage on the registered surfaces,
// the surfaces themselves and every attribute and block at any depth, and
// lists the ones that don't name the major after released as the version
// they're removed in.
func Deprecations(ctx context.Context, released int) (int, []string) {
	surfaces := append(docSurfaces(ctx), listAndActionSurfaces(ctx, New("docs")())...)
	total := 0
	var gaps []string
	for _, s := range surfaces {
		n, g := deprecationGaps(s, released)
		total += n
		gaps = append(gaps, g...)
	}
	slices.Sort(gaps)
	return total, gaps
}

// listAndActionSurfaces reads the list resources and actions a provider
// registers, which the documentation surfaces don't cover. Both schemas carry
// a DeprecationMessage of their own and on their attributes.
func listAndActionSurfaces(ctx context.Context, p fwprovider.Provider) []docSurface {
	var out []docSurface
	if lp, ok := p.(fwprovider.ProviderWithListResources); ok {
		for _, newList := range lp.ListResources(ctx) {
			l := newList()
			var md resource.MetadataResponse
			l.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
			var sr list.ListResourceSchemaResponse
			l.ListResourceConfigSchema(ctx, list.ListResourceSchemaRequest{}, &sr)
			out = append(out, docSurface{
				kind: "list resource", name: md.TypeName,
				attributes: reflect.ValueOf(sr.Schema.Attributes), blocks: reflect.ValueOf(sr.Schema.Blocks),
				deprecation: sr.Schema.DeprecationMessage,
			})
		}
	}
	if ap, ok := p.(fwprovider.ProviderWithActions); ok {
		for _, newAction := range ap.Actions(ctx) {
			a := newAction()
			var md action.MetadataResponse
			a.Metadata(ctx, action.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
			var sr action.SchemaResponse
			a.Schema(ctx, action.SchemaRequest{}, &sr)
			out = append(out, docSurface{
				kind: "action", name: md.TypeName,
				attributes: reflect.ValueOf(sr.Schema.Attributes), blocks: reflect.ValueOf(sr.Schema.Blocks),
				deprecation: sr.Schema.DeprecationMessage,
			})
		}
	}
	return out
}

func deprecationGaps(s docSurface, released int) (int, []string) {
	next := released + 1
	count := 0
	var gaps []string
	visit := func(path, message string) {
		if message == "" {
			return
		}
		count++
		m := removalVersion.FindStringSubmatch(message)
		// A major too large for an int reads as none named.
		major := 0
		if m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				major = n
			}
		}
		switch {
		case major == 0:
			gaps = append(gaps, fmt.Sprintf("%s: deprecated, and its message %q doesn't name the major version "+
				"it's removed in, which is \"removed in v%d.0.0\"", path, message, next))
		case major > next:
			gaps = append(gaps, fmt.Sprintf("%s: deprecated until v%d.0.0, which is too far: the policy removes it "+
				"in the next major, v%d.0.0", path, major, next))
		case major < next:
			gaps = append(gaps, fmt.Sprintf("%s: deprecated until v%d.0.0, which is already out: the next major is "+
				"v%d.0.0", path, major, next))
		}
	}
	prefix := s.kind + " " + s.name
	visit(prefix, s.deprecation)
	for _, m := range []reflect.Value{s.attributes, s.blocks} {
		walkDocumented(m, prefix+".", false, func(path string, v reflect.Value, _ bool) {
			if f := v.FieldByName("DeprecationMessage"); f.IsValid() && f.Kind() == reflect.String {
				visit(path, f.String())
			}
		})
	}
	slices.Sort(gaps)
	return count, gaps
}
