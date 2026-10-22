package k0rdent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"go.yaml.in/yaml/v3"
)

// These tests hold the generated client to the prepared spec it was generated
// from, one operation at a time, through the real ClientWithResponses and a
// local server. Nothing here names an operation: the spec's operationId is
// upper-cased the way oapi-codegen does it and found by reflection, so a new
// operation is covered the day it is generated.
//
// knownFailures lists the mismatches the tests have found and that are not
// fixed yet, keyed by subtest name: "<operationId> request <body>" or
// "<operationId> <status> <body>". A listed case must still fail. Once it
// passes, the entry has to go, so the table cannot outlive the bug.
var knownFailures = map[string]string{}

// TestConformance_EveryOperationHasAClientMethod checks the mapping both
// ways, so an operation the generator skipped and a method the spec no longer
// declares both fail here rather than as a confusing subtest below.
func TestConformance_EveryOperationHasAClientMethod(t *testing.T) {
	spec := loadSpec(t)
	client := reflect.TypeOf(&ClientWithResponses{})

	want := map[string]bool{}
	for _, op := range spec.operations() {
		want[op.method()] = true
		if _, ok := client.MethodByName(op.method()); !ok {
			t.Errorf("%s %s: no ClientWithResponses.%s", op.verb, op.path, op.method())
		}
	}
	for i := range client.NumMethod() {
		name := client.Method(i).Name
		if strings.HasSuffix(name, "WithResponse") && !strings.HasSuffix(name, "WithBodyWithResponse") && !want[name] {
			t.Errorf("ClientWithResponses.%s has no operation in the spec", name)
		}
	}
}

// TestConformance_Requests sends every operation with every parameter set and
// checks what reached the wire: the spec's method, the path template filled in
// order, the query and header names the spec declares, each carrying the value
// set for it, and a JSON body that keeps every field of each body bodies
// builds for it.
func TestConformance_Requests(t *testing.T) {
	spec := loadSpec(t)
	srv := newRecorder(t)

	for _, op := range spec.operations() {
		for _, body := range op.requestBodies(spec) {
			check(t, op.id+" request "+body.name, func(t testing.TB) {
				srv.reply(http.StatusNoContent, nil)
				send := body.value
				args, wantPath, takesBody, params := op.args(t, spec, send)
				if _, err := op.call(t, srv.client, args); err != nil {
					t.Fatalf("call: %v", err)
				}
				req, sentBytes := srv.last()

				if req.Method != strings.ToUpper(op.verb) {
					t.Errorf("method = %s, spec says %s", req.Method, strings.ToUpper(op.verb))
				}
				if req.URL.EscapedPath() != wantPath {
					t.Errorf("path = %s, want %s from %s", req.URL.EscapedPath(), wantPath, op.path)
				}
				query := req.URL.Query()
				var gotQuery []string
				for k := range query {
					gotQuery = append(gotQuery, k)
				}
				if wantQuery := op.paramNames(spec, "query"); !sameSet(gotQuery, wantQuery) {
					t.Errorf("query names = %v, spec declares %v", sorted(gotQuery), sorted(wantQuery))
				}
				for _, q := range op.paramNames(spec, "query") {
					if want := wireValues(t, params, "form", q); !slices.Equal(query[q], want) {
						t.Errorf("query %s = %q, want %q, the value set for it", q, query[q], want)
					}
				}
				for _, h := range op.paramNames(spec, "header") {
					want := strings.Join(wireValues(t, params, "json", h), ",")
					if got := req.Header.Values(h); !slices.Equal(got, []string{want}) {
						t.Errorf("header %s = %q, want %q, the value set for it", h, got, want)
					}
				}

				if send != nil {
					if ct := req.Header.Get("Content-Type"); ct != "application/json" {
						t.Errorf("Content-Type = %q, want application/json", ct)
					}
					var sent any
					if err := decodeJSON(sentBytes, &sent); err != nil {
						t.Fatalf("body is not JSON: %v: %s", err, sentBytes)
					}
					for _, loss := range lost(send, sent, "") {
						t.Errorf("request body lost %s\nmeant: %s\nsent:  %s", loss, mustJSON(send), sentBytes)
					}
				} else if takesBody {
					t.Errorf("the method takes a body the spec does not declare as JSON")
				}
			})
		}
	}
}

// TestConformance_Responses serves every body bodies builds for each JSON
// response, including the spec's examples, and checks that the generated
// parser decodes it into the JSON<status> field, and that marshalling that
// field keeps every field the body had.
func TestConformance_Responses(t *testing.T) {
	spec := loadSpec(t)
	srv := newRecorder(t)
	served := 0

	for _, op := range spec.operations() {
		for _, status := range sortedKeys(op.responses) {
			media := spec.resolve(op.responses[status]).sub("content").sub("application/json")
			if media == nil {
				continue
			}
			for _, ex := range bodies(spec, media) {
				served++
				check(t, fmt.Sprintf("%s %s %s", op.id, status, ex.name), func(t testing.TB) {
					code, err := strconv.Atoi(status)
					if err != nil {
						t.Fatalf("status %q is not a number, so teach this test the generator's name for it", status)
					}
					raw := mustJSON(ex.value)
					srv.reply(code, raw)
					args, _, _, _ := op.args(t, spec, op.requestBodies(spec)[0].value)
					resp, err := op.call(t, srv.client, args)
					if err != nil {
						t.Fatalf("parse: %v\nexample: %s", err, raw)
					}
					field := resp.Elem().FieldByName("JSON" + status)
					if !field.IsValid() {
						t.Fatalf("%s has no JSON%s field", resp.Elem().Type().Name(), status)
					}
					if field.IsNil() {
						t.Fatalf("JSON%s is nil after a %d with a JSON body", status, code)
					}
					again, err := json.Marshal(field.Interface())
					if err != nil {
						t.Fatalf("re-marshal %s: %v", field.Type(), err)
					}
					var got any
					if err := decodeJSON(again, &got); err != nil {
						t.Fatalf("re-marshal is not JSON: %v", err)
					}
					for _, loss := range lost(ex.value, got, "") {
						t.Errorf("%s lost %s\nexample:    %s\nre-marshal: %s", field.Type(), loss, raw, again)
					}
				})
			}
		}
	}
	t.Logf("served %d response bodies", served)
}

// check runs fn as a subtest and holds it against knownFailures.
func check(t *testing.T, name string, fn func(t testing.TB)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		reason, known := knownFailures[name]
		if !known {
			fn(t)
			return
		}
		probe := &probeTB{TB: t}
		func() {
			defer func() {
				if r := recover(); r != nil && r != errProbeStop {
					panic(r)
				}
			}()
			fn(probe)
		}()
		if !probe.failed {
			t.Fatalf("passes now; remove it from knownFailures (%s)", reason)
		}
		t.Skipf("known failure: %s", reason)
	})
}

// probeTB records a failure instead of reporting it, so a known failure can be
// checked to still fail without failing the run.
type probeTB struct {
	testing.TB
	failed bool
}

var errProbeStop = new(int)

func (p *probeTB) Errorf(string, ...any) { p.failed = true }
func (p *probeTB) Error(...any)          { p.failed = true }
func (p *probeTB) Fail()                 { p.failed = true }
func (p *probeTB) Failed() bool          { return p.failed }
func (p *probeTB) FailNow() {
	p.failed = true
	panic(errProbeStop)
}
func (p *probeTB) Fatal(...any)          { p.FailNow() }
func (p *probeTB) Fatalf(string, ...any) { p.FailNow() }

// lost lists what got dropped from want: a key that is missing, or a value
// that changed. A key whose example value is null may be absent, because an
// optional field is a nil pointer with omitempty and both decode to the same
// Go value. Keys that got added are not a loss.
func lost(want, got any, at string) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s (object became %v)", orRoot(at), got)}
		}
		var out []string
		for _, k := range sortedKeys(w) {
			gv, present := g[k]
			switch {
			case !present && w[k] == nil:
			case !present:
				out = append(out, at+"."+k)
			default:
				out = append(out, lost(w[k], gv, at+"."+k)...)
			}
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return []string{fmt.Sprintf("%s (array of %d became %v)", orRoot(at), len(w), got)}
		}
		var out []string
		for i := range w {
			out = append(out, lost(w[i], g[i], fmt.Sprintf("%s[%d]", at, i))...)
		}
		return out
	default:
		if !sameScalar(want, got) {
			return []string{fmt.Sprintf("%s (%v became %v)", orRoot(at), want, got)}
		}
		return nil
	}
}

// sameScalar compares numbers by value and timestamps by instant, since a Go
// time.Time or int re-marshals a different spelling of the same value.
func sameScalar(want, got any) bool {
	if want == got {
		return true
	}
	if w, ok := want.(json.Number); ok {
		g, ok := got.(json.Number)
		wf, _ := w.Float64()
		gf, _ := g.Float64()
		return ok && wf == gf
	}
	ws, wok := want.(string)
	gs, gok := got.(string)
	if wok && gok {
		wt, werr := time.Parse(time.RFC3339Nano, ws)
		gt, gerr := time.Parse(time.RFC3339Nano, gs)
		return werr == nil && gerr == nil && wt.Equal(gt)
	}
	return false
}

func orRoot(at string) string {
	if at == "" {
		return "the body"
	}
	return at
}

type node map[string]any

func (n node) sub(key string) node {
	m, _ := n[key].(map[string]any)
	return m
}

type specDoc struct{ root node }

// loadSpec reads the prepared spec into plain values. It walks the YAML nodes
// itself rather than decoding into `any`, because that decoder turns an
// unquoted date into a time.Time, and the examples have to reach the wire
// exactly as the spec writes them.
func loadSpec(t testing.TB) specDoc {
	t.Helper()
	raw, err := os.ReadFile("spec/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return specDoc{root: plain(doc.Content[0]).(map[string]any)}
}

func plain(n *yaml.Node) any {
	switch n.Kind {
	case yaml.AliasNode:
		return plain(n.Alias)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			m[n.Content[i].Value] = plain(n.Content[i+1])
		}
		return m
	case yaml.SequenceNode:
		s := make([]any, len(n.Content))
		for i, c := range n.Content {
			s[i] = plain(c)
		}
		return s
	}
	switch n.Tag {
	case "!!null":
		return nil
	case "!!bool":
		return n.Value == "true"
	case "!!int", "!!float":
		return json.Number(n.Value)
	}
	return n.Value
}

// resolve follows a local $ref until it reaches the object it names.
func (s specDoc) resolve(v any) node {
	n, ok := v.(node)
	if !ok {
		n, _ = v.(map[string]any)
	}
	for {
		ref, ok := n["$ref"].(string)
		if !ok {
			return n
		}
		cur := s.root
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			cur = cur.sub(part)
		}
		n = cur
	}
}

type operation struct {
	id, verb, path string
	params         []any
	body           node
	responses      node
}

func (s specDoc) operations() []operation {
	var ops []operation
	paths := s.root.sub("paths")
	for _, path := range sortedKeys(paths) {
		item := paths.sub(path)
		for _, verb := range []string{"get", "put", "post", "delete", "patch", "head", "options"} {
			op := item.sub(verb)
			if op == nil {
				continue
			}
			params, _ := item["parameters"].([]any)
			own, _ := op["parameters"].([]any)
			ops = append(ops, operation{
				id:        op["operationId"].(string),
				verb:      verb,
				path:      path,
				params:    append(slices.Clone(params), own...),
				body:      s.resolve(op["requestBody"]),
				responses: op.sub("responses"),
			})
		}
	}
	return ops
}

// method is the name oapi-codegen gives the operation on ClientWithResponses.
func (op operation) method() string {
	return strings.ToUpper(op.id[:1]) + op.id[1:] + "WithResponse"
}

// pathValues is a value for each path parameter, in template order: the
// parameter's example, or one built from its schema, so a builder that holds
// values to the contract's patterns and enums accepts it.
func (op operation) pathValues(s specDoc) []any {
	var values []any
	for _, m := range pathParam.FindAllStringSubmatch(op.path, -1) {
		for _, p := range op.params {
			if r := s.resolve(p); r["in"] == "path" && r["name"] == m[1] {
				v, ok := r["example"]
				if !ok {
					v = synth{s, false}.instance(r["schema"], 0)
				}
				values = append(values, v)
			}
		}
	}
	return values
}

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

func (op operation) paramNames(s specDoc, in string) []string {
	var names []string
	for _, p := range op.params {
		if r := s.resolve(p); r["in"] == in {
			names = append(names, r["name"].(string))
		}
	}
	return names
}

// requestBodies are the JSON bodies to send, or a single nil one when the
// spec declares none.
func (op operation) requestBodies(s specDoc) []example {
	media := op.body.sub("content").sub("application/json")
	if media == nil {
		return []example{{"without body", nil}}
	}
	return bodies(s, media)
}

type example struct {
	name  string
	value any
}

// examples returns a media type's `example`, or each of its named
// `examples`, which may be $refs into components.
func examples(s specDoc, media node) []example {
	if v, ok := media["example"]; ok {
		return []example{{"example", v}}
	}
	named := media.sub("examples")
	var out []example
	for _, name := range sortedKeys(named) {
		out = append(out, example{name, s.resolve(named[name])["value"]})
	}
	return out
}

// bodies is what to send for a media type: its examples, or one value built
// from its schema when it has none, so a body without an example, like
// mintToken's, is still checked. A second value built from the schema sets
// every field to its zero value, or null where the schema allows it. That is
// the case omitempty and a non-pointer field get wrong.
func bodies(s specDoc, media node) []example {
	out := examples(s, media)
	if len(out) == 0 {
		out = append(out, example{"schema", synth{s, false}.instance(media["schema"], 0)})
	}
	return append(out, example{"zeros", synth{s, true}.instance(media["schema"], 0)})
}

// synth builds a value that satisfies a schema, setting every property and
// taking the first branch of a oneOf or anyOf. It prefers the spec's own
// examples, enums and consts, so the values are ones the API documents. With
// zero set, it uses empty values and null instead of examples, but keeps
// enums and formats, which the generated types check on decode.
type synth struct {
	spec specDoc
	zero bool
}

func (y synth) instance(v any, depth int) any {
	sc := y.spec.resolve(v)
	if depth > 16 {
		return nil
	}
	if ex, ok := sc["example"]; ok && !y.zero {
		return ex
	}
	if c, ok := sc["const"]; ok {
		return c
	}
	if enum, _ := sc["enum"].([]any); len(enum) > 0 {
		return enum[0]
	}
	if all, _ := sc["allOf"].([]any); len(all) > 0 {
		merged := map[string]any{}
		for _, part := range all {
			switch v := y.instance(part, depth+1).(type) {
			case nil: // a part that only adds constraints, like `required`
			case map[string]any:
				maps.Copy(merged, v)
			default: // an allOf that wraps a scalar to annotate it
				return v
			}
		}
		return merged
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		if branches, _ := sc[key].([]any); len(branches) > 0 {
			return y.instance(branches[0], depth+1)
		}
	}
	typ, nullable := schemaType(sc)
	if nullable && y.zero {
		return nil
	}
	format, hasFormat := sampleFormats[fmt.Sprint(sc["format"])]
	switch {
	case typ == "object":
		m := map[string]any{}
		props := sc.sub("properties")
		for _, k := range sortedKeys(props) {
			m[k] = y.instance(props[k], depth+1)
		}
		if ap := sc.sub("additionalProperties"); len(props) == 0 && ap != nil && !y.zero {
			m["key"] = y.instance(ap, depth+1)
		}
		return m
	case typ == "array" && y.zero:
		return []any{}
	case typ == "array":
		return []any{y.instance(sc["items"], depth+1)}
	case typ == "integer" || typ == "number":
		if min, ok := sc["minimum"].(json.Number); ok && !y.zero {
			return min
		}
		return json.Number(map[bool]string{false: "1", true: "0"}[y.zero])
	case typ == "boolean":
		return !y.zero
	case typ == "string" && hasFormat:
		return format
	case typ == "string" && y.zero:
		return ""
	case typ == "string" && sc["pattern"] != nil:
		if re, err := syntax.Parse(sc["pattern"].(string), syntax.Perl); err == nil {
			return matching(re)
		}
		fallthrough
	case typ == "string":
		n, _ := strconv.Atoi(fmt.Sprint(sc["minLength"]))
		return strings.Repeat("s", max(n, 1))
	}
	return nil
}

var sampleFormats = map[string]string{
	"date-time":     "2026-01-02T03:04:05Z",
	"date":          "2026-01-02",
	"uuid":          "00000000-0000-4000-8000-000000000001",
	"email":         "someone@example.com",
	"uri":           "https://example.com/x",
	"uri-reference": "/x",
	"byte":          "aGk=",
}

// matching returns the shortest string re matches, taking the first branch
// and the first rune of each class. The contract's patterns are plain
// prefixes and classes, which this covers.
func matching(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		return string(re.Rune)
	case syntax.OpCharClass:
		return string(re.Rune[0])
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return "a"
	case syntax.OpCapture, syntax.OpPlus:
		return matching(re.Sub[0])
	case syntax.OpRepeat:
		return strings.Repeat(matching(re.Sub[0]), re.Min)
	case syntax.OpConcat:
		var b strings.Builder
		for _, sub := range re.Sub {
			b.WriteString(matching(sub))
		}
		return b.String()
	case syntax.OpAlternate:
		return matching(re.Sub[0])
	}
	return "" // anchors, empty matches, and the optional parts of a star or quest
}

// schemaType reads `type`, which 3.1 also allows as a list such as
// [string, "null"]. An untyped schema with properties is an object.
func schemaType(sc node) (typ string, nullable bool) {
	switch t := sc["type"].(type) {
	case string:
		return t, false
	case []any:
		for _, e := range t {
			if e == "null" || e == nil {
				nullable = true
			} else {
				typ = e.(string)
			}
		}
		return typ, nullable
	}
	if sc["properties"] != nil {
		return "object", false
	}
	return "", false
}

// args builds the arguments for op's WithResponse method: a value the spec
// allows for each path parameter in template order, every query and header
// parameter set, and body decoded into the body type. It returns the path
// those values should produce, whether the method takes a body, and the
// Params struct it filled, if the method has one.
func (op operation) args(t testing.TB, s specDoc, body any) (args []reflect.Value, wantPath string, hasBody bool, params reflect.Value) {
	t.Helper()
	m, ok := reflect.TypeOf(&ClientWithResponses{}).MethodByName(op.method())
	if !ok {
		t.Fatalf("no ClientWithResponses.%s", op.method())
	}
	in := m.Type // receiver, ctx, path params..., [params], [body], reqEditors
	wantPath = op.path
	pathValues := op.pathValues(s)
	args = []reflect.Value{reflect.ValueOf(context.Background())}
	for i := 2; i < in.NumIn()-1; i++ {
		typ := in.In(i)
		v := reflect.New(typ).Elem()
		switch {
		case i-2 < len(pathValues):
			value := pathValues[i-2]
			if err := json.Unmarshal(mustJSON(value), v.Addr().Interface()); err != nil {
				t.Fatalf("path value %v does not decode into %s: %v", value, typ, err)
			}
			open := strings.Index(wantPath, "{")
			end := strings.Index(wantPath, "}")
			wantPath = wantPath[:open] + url.PathEscape(fmt.Sprint(value)) + wantPath[end+1:]
		case typ.Kind() == reflect.Pointer && strings.HasSuffix(typ.Elem().Name(), "Params"):
			setSample(v, 1)
			params = v.Elem()
		default:
			hasBody = true
			if body != nil {
				if err := json.Unmarshal(mustJSON(body), v.Addr().Interface()); err != nil {
					t.Fatalf("request body does not decode into %s: %v", typ, err)
				}
			}
		}
		args = append(args, v)
	}
	if strings.Contains(wantPath, "{") {
		t.Fatalf("%s takes fewer path arguments than %s has", op.method(), op.path)
	}
	return args, wantPath, hasBody, params
}

// wireValues is what the Params field the spec's parameter name maps to
// should put on the wire, one string per value: the field is found by its
// form (query) or json (header) tag, and a slice sends each element. The
// encoding is written out here, not borrowed from the runtime, so a generated
// call that passed the wrong field, or nothing, can't agree with it.
func wireValues(t testing.TB, params reflect.Value, tag, name string) []string {
	t.Helper()
	if !params.IsValid() {
		t.Fatalf("%s %s is declared, and the method has no Params", tag, name)
	}
	for i := range params.NumField() {
		key, _, _ := strings.Cut(params.Type().Field(i).Tag.Get(tag), ",")
		if key == name {
			return wire(params.Field(i))
		}
	}
	t.Fatalf("%s has no field tagged %s:%q", params.Type(), tag, name)
	return nil
}

func wire(v reflect.Value) []string {
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if tm, ok := v.Interface().(time.Time); ok {
		return []string{tm.Format(time.RFC3339Nano)}
	}
	if str, ok := v.Interface().(fmt.Stringer); ok {
		return []string{str.String()}
	}
	switch v.Kind() {
	case reflect.Slice:
		var out []string
		for i := range v.Len() {
			out = append(out, wire(v.Index(i))...)
		}
		return out
	case reflect.Bool:
		return []string{strconv.FormatBool(v.Bool())}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return []string{strconv.FormatInt(v.Int(), 10)}
	}
	return []string{fmt.Sprint(v.Interface())}
}

func (op operation) call(t testing.TB, c *ClientWithResponses, args []reflect.Value) (reflect.Value, error) {
	t.Helper()
	out := reflect.ValueOf(c).MethodByName(op.method()).Call(args)
	err, _ := out[1].Interface().(error)
	return out[0], err
}

// setSample fills v with a non-zero value: n for numbers, "p<n>" for strings,
// and one element for a slice. Pointers and nested structs are filled too, so
// every optional parameter of a Params struct is sent, field i with n+i, so no
// two parameters of one kind carry the same value.
func setSample(v reflect.Value, n int) {
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		setSample(v.Elem(), n)
	case reflect.String:
		if v.Type() == reflect.TypeOf(openapi_types.Email("")) {
			v.SetString(fmt.Sprintf("p%d@example.com", n))
			return
		}
		v.SetString(fmt.Sprintf("p%d", n))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		setSample(v.Index(0), n)
	case reflect.Array: // openapi_types.UUID
		v.Index(v.Len() - 1).SetUint(uint64(n))
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				setSample(v.Field(i), n+i)
			}
		}
	}
}

// recorder is a server that keeps the last request and answers with whatever
// reply set.
type recorder struct {
	client *ClientWithResponses

	mu     sync.Mutex
	req    *http.Request
	body   []byte
	status int
	answer []byte
}

func newRecorder(t *testing.T) *recorder {
	r := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.req = req
		r.body, _ = io.ReadAll(req.Body)
		if r.answer != nil {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(r.status)
		_, _ = w.Write(r.answer)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.client = c
	return r
}

func (r *recorder) reply(status int, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.answer = status, body
}

func (r *recorder) last() (*http.Request, []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.req, r.body
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func decodeJSON(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d.Decode(v)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func sorted(s []string) []string { return slices.Sorted(slices.Values(s)) }

func sameSet(a, b []string) bool { return slices.Equal(sorted(a), sorted(b)) }
