// Command clientlimits writes the contract's limits on values as Go constants in
// the client package.
//
// oapi-codegen keeps a schema's enum, as typed constants with a generated
// Values() list, and drops every other limit: a pattern, a length, a range, a
// count. A consumer that wants
// to refuse a value before the API does would otherwise copy those numbers out
// of the spec by hand, and the copy drifts the day the contract moves. This
// reads them from internal/k0rdent/spec/openapi.yaml, the same file the
// generator reads, so they move with every sync.
//
// It covers every property of every component schema, and of every request
// body declared in place, at any depth, and every path parameter of every
// operation, and these keywords: pattern, minLength, maxLength, minimum,
// maximum, minItems and maxItems. Each is one constant, named for where it
// sits and what it limits:
//
//	FileStoreCreateRequest.capacityGiB  maximum  FileStoreCreateRequestCapacityGiBMaximum
//	SshUser.sshPublicKeys[]             maxLength SshUserSshPublicKeysItemMaxLength
//	getInstance path id                 pattern  GetInstancePathIdPattern
//	createServiceAccount body.id        pattern  CreateServiceAccountJSONBodyIdPattern
//
// A pattern is the contract's, verbatim. OpenAPI patterns are ECMA-262, and
// the few Go's regexp can't compile as written are marked where they're
// declared.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

func main() {
	var in, out string
	flag.StringVar(&in, "in", "spec/openapi.yaml", "the contract the client is generated from")
	flag.StringVar(&out, "out", "limits.gen.go", "generated limits")
	flag.Parse()

	if err := run(in, out); err != nil {
		fmt.Fprintf(os.Stderr, "clientlimits: %v\n", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", in, err)
	}
	locs, err := collect(doc)
	if err != nil {
		return err
	}
	src, err := render(locs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, src, 0o644); err != nil {
		return err
	}
	fmt.Printf("clientlimits: wrote %d limits at %d locations to %s\n", count(locs), len(locs), out)
	return nil
}

// keywords are the limits written out, in the order they're declared.
var keywords = []struct{ spec, suffix string }{
	{"pattern", "Pattern"},
	{"minLength", "MinLength"},
	{"maxLength", "MaxLength"},
	{"minimum", "Minimum"},
	{"maximum", "Maximum"},
	{"minItems", "MinItems"},
	{"maxItems", "MaxItems"},
}

// location is one limited value: where the contract declares it, the Go name
// its constants start with, and the keywords that limit it.
type location struct {
	where  string // as the spec spells it, such as "SshUser.sshPublicKeys[]"
	name   string
	limits map[string]any
}

func collect(doc map[string]any) ([]location, error) {
	byName := map[string]*location{}
	add := func(where, name string, schema map[string]any) error {
		limits := map[string]any{}
		for _, k := range keywords {
			if v, ok := schema[k.spec]; ok {
				limits[k.spec] = v
			}
		}
		if len(limits) == 0 {
			return nil
		}
		prev, ok := byName[name]
		if !ok {
			byName[name] = &location{where: where, name: name, limits: limits}
			return nil
		}
		if prev.where != where {
			return fmt.Errorf("%s and %s would both be named %s", prev.where, where, name)
		}
		// allOf and oneOf can declare one property twice. That's one location
		// when they agree, and two values for one name when they don't.
		for k, v := range limits {
			if old, seen := prev.limits[k]; seen && fmt.Sprint(old) != fmt.Sprint(v) {
				return fmt.Errorf("%s declares %s twice, as %v and %v, so %s%s has no one value",
					where, k, old, v, name, suffixOf(k))
			}
			prev.limits[k] = v
		}
		return nil
	}

	components := asMap(doc["components"])
	schemas := asMap(components["schemas"])
	var walk func(where, name string, node any, depth int) error
	walk = func(where, name string, node any, depth int) error {
		s := asMap(node)
		if s == nil || s["$ref"] != nil {
			// A reference's limits are the referenced schema's, declared there.
			return nil
		}
		if depth > 32 {
			return fmt.Errorf("%s: nested more than 32 deep", where)
		}
		if err := add(where, name, s); err != nil {
			return err
		}
		for _, key := range []string{"allOf", "oneOf", "anyOf"} {
			for _, sub := range asList(s[key]) {
				if err := walk(where, name, sub, depth+1); err != nil {
					return err
				}
			}
		}
		props := asMap(s["properties"])
		for _, p := range sortedKeys(props) {
			if err := walk(where+"."+p, name+goName(p), props[p], depth+1); err != nil {
				return err
			}
		}
		if items, ok := s["items"]; ok {
			if err := walk(where+"[]", name+"Item", items, depth+1); err != nil {
				return err
			}
		}
		if extra, ok := s["additionalProperties"].(map[string]any); ok {
			if err := walk(where+"{}", name+"Value", extra, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for _, n := range sortedKeys(schemas) {
		if err := walk(n, goName(n), schemas[n], 0); err != nil {
			return nil, err
		}
	}

	paths := asMap(doc["paths"])
	for _, p := range sortedKeys(paths) {
		item := asMap(paths[p])
		for _, method := range []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"} {
			op := asMap(item[method])
			if op == nil {
				continue
			}
			id, _ := op["operationId"].(string) //nolint:errcheck // a missing id reads as empty
			if id == "" {
				return nil, fmt.Errorf("%s %s has no operationId", method, p)
			}
			for _, param := range pathParams(doc, item, op) {
				pname, _ := param["name"].(string) //nolint:errcheck // as id
				schema := resolve(doc, param["schema"])
				if err := add(id+" path "+pname, goName(id)+"Path"+goName(pname), schema); err != nil {
					return nil, err
				}
			}
			// A body declared in place has no component schema to be named
			// for. oapi-codegen names its type <Operation>JSONBody.
			body := resolve(doc, op["requestBody"])
			media := asMap(asMap(body["content"])["application/json"])
			if err := walk(id+" body", goName(id)+"JSONBody", media["schema"], 0); err != nil {
				return nil, err
			}
		}
	}

	out := make([]location, 0, len(byName))
	for _, l := range byName {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// pathParams are an operation's path parameters: the path item's, then the
// operation's own, which replace one of the same name.
func pathParams(doc, item, op map[string]any) []map[string]any {
	var names []string
	byName := map[string]map[string]any{}
	for _, list := range [][]any{asList(item["parameters"]), asList(op["parameters"])} {
		for _, p := range list {
			p := resolve(doc, p)
			if p["in"] != "path" {
				continue
			}
			name, _ := p["name"].(string) //nolint:errcheck // a missing name reads as empty
			if _, seen := byName[name]; !seen {
				names = append(names, name)
			}
			byName[name] = p
		}
	}
	out := make([]map[string]any, len(names))
	for i, n := range names {
		out[i] = byName[n]
	}
	return out
}

// resolve follows local $refs to the node they name.
func resolve(doc map[string]any, node any) map[string]any {
	for range 32 {
		m := asMap(node)
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		var cur any = doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			cur = asMap(cur)[part]
		}
		node = cur
	}
	return nil
}

func render(locs []location) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`// Code generated by internal/tools/clientlimits from spec/openapi.yaml. DO NOT EDIT.

package k0rdent

// The contract's limits on values, one per keyword, named for the schema
// property or the operation's path parameter they limit. See internal/tools/clientlimits.
`)
	for _, l := range locs {
		fmt.Fprintf(&b, "\n// %s\n", l.where)
		var consts []string
		for _, k := range keywords {
			v, ok := l.limits[k.spec]
			if !ok {
				continue
			}
			name := l.name + k.suffix
			lit, err := literal(l.where, k.spec, v)
			if err != nil {
				return nil, err
			}
			line := fmt.Sprintf("\t%s = %s\n", name, lit)
			if s, isPattern := v.(string); isPattern && k.spec == "pattern" {
				if _, err := regexp.Compile(s); err != nil {
					line = "\t// Go's regexp can't compile this one as written.\n" + line
				}
			}
			consts = append(consts, line)
		}
		if len(consts) > 0 {
			b.WriteString("const (\n" + strings.Join(consts, "") + ")\n")
		}
	}
	return format.Source(b.Bytes())
}

func literal(where, keyword string, v any) (string, error) {
	switch v := v.(type) {
	case string:
		if keyword == "pattern" && !strings.Contains(v, "`") {
			return "`" + v + "`", nil
		}
		return strconv.Quote(v), nil
	case int:
		return strconv.Itoa(v), nil
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	}
	return "", fmt.Errorf("%s: %s is a %T, which has no Go constant", where, keyword, v)
}

// goName is oapi-codegen's ToCamelCase, so a constant starts with the name of
// the type or field it limits: separators dropped, the letter after each one
// upper-cased, the first letter upper-cased.
func goName(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			if up {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
			up = false
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			up = false
		default:
			up = true
		}
	}
	return b.String()
}

func suffixOf(spec string) string {
	i := slices.IndexFunc(keywords, func(k struct{ spec, suffix string }) bool { return k.spec == spec })
	return keywords[i].suffix
}

func count(locs []location) int {
	n := 0
	for _, l := range locs {
		n += len(l.limits)
	}
	return n
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any) //nolint:errcheck // a node of another shape reads as empty
	return m
}

func asList(v any) []any {
	l, _ := v.([]any) //nolint:errcheck // as asMap
	return l
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
