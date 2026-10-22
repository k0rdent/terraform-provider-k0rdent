package k0rdent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.yaml.in/yaml/v3"
)

// seenRequest is what a transport could tell about one request.
type seenRequest struct {
	method, path, operation string
	tagged                  bool
}

// recordingTransport notes each request on its way through.
type recordingTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	seen []seenRequest
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	id, ok := OperationIDFromContext(req.Context())
	r.mu.Lock()
	r.seen = append(r.seen, seenRequest{req.Method, req.URL.Path, id, ok})
	r.mu.Unlock()
	return r.next.RoundTrip(req)
}

// A transport underneath New sees the operation id of the call that sent each
// request, the mint New makes on its own included, and the caller's context
// values still reach it.
func TestTransportSeesTheOperationOfEachCall(t *testing.T) {
	f := &fakeAPI{}
	srv := f.server(t)
	rec := &recordingTransport{next: ownPool()}
	c := testClient(t, srv, WithTransport(rec))

	type callerKey struct{}
	ctx := context.WithValue(context.Background(), callerKey{}, "kept")
	var editorSaw string
	keep := func(ctx context.Context, req *http.Request) error {
		editorSaw, _ = OperationIDFromContext(ctx)
		if req.Context().Value(callerKey{}) != "kept" {
			t.Error("the caller's context value did not reach the request")
		}
		return nil
	}
	resp, err := c.ListClusterTypesWithResponse(ctx, "r1", nil, keep)
	if err != nil {
		t.Fatalf("ListClusterTypes: %v", err)
	}
	if err := CheckResponse(resp.HTTPResult()); err != nil {
		t.Fatalf("ListClusterTypes: %v", err)
	}

	want := []seenRequest{
		{http.MethodPost, "/v1/regions/global/auth/token", "mintToken", true},
		{http.MethodGet, "/v1/regions/r1/compute/cluster-types", "listClusterTypes", true},
	}
	if !reflect.DeepEqual(rec.seen, want) {
		t.Errorf("the transport saw\n%+v\nwant\n%+v", rec.seen, want)
	}
	if editorSaw != "listClusterTypes" {
		t.Errorf("a request editor read operation %q, want listClusterTypes", editorSaw)
	}
}

func TestOperationIDFromContextIsFalseOffAGeneratedCall(t *testing.T) {
	if id, ok := OperationIDFromContext(context.Background()); ok {
		t.Errorf("OperationIDFromContext on a bare context = %q, true", id)
	}
}

var errStopBeforeSending = errors.New("stopped before sending")

// doerFunc lets a test stand in for the HTTP client.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// Every generated method tags its request with an id the table has, and the
// request goes out with that entry's method on that entry's path. Every entry
// is reached by some method, so the table holds nothing the client can't call.
// The methods are found by reflection, so one the contract adds is covered
// without editing this test.
func TestEveryMethodTagsTheOperationItSends(t *testing.T) {
	var got *http.Request
	c, err := NewClient("https://api.invalid", WithHTTPClient(doerFunc(func(req *http.Request) (*http.Response, error) {
		got = req
		return nil, errStopBeforeSending
	})))
	if err != nil {
		t.Fatal(err)
	}

	reached := map[string]bool{}
	v := reflect.ValueOf(c)
	for i := 0; i < v.NumMethod(); i++ {
		name := v.Type().Method(i).Name
		m := v.Method(i)
		if m.Type().NumIn() == 0 || m.Type().In(0) != reflect.TypeFor[context.Context]() {
			continue
		}
		got = nil
		// A typed body whose zero value won't encode, like an empty email,
		// fails before sending. Its WithBody form still sends, and the
		// coverage check below fails if no method reaches the operation.
		args := placeholderArgs(m.Type())
		err, _ := m.Call(args)[1].Interface().(error)
		// The contract's own rule can refuse a placeholder, like an instance
		// id's pattern. The refusal names the value, so that argument is
		// swapped for one the rule takes and the call is made again.
		for range len(args) * (len(patternFillers) + 1) {
			var pe *PathParamError
			if !errors.As(err, &pe) || !fitRefusedArg(args, pe) {
				break
			}
			err, _ = m.Call(args)[1].Interface().(error)
		}
		if !errors.Is(err, errStopBeforeSending) {
			continue
		}
		id, ok := OperationIDFromContext(got.Context())
		if !ok {
			t.Errorf("%s sent a request with no operation id", name)
			continue
		}
		op, ok := Operations[id]
		if !ok {
			t.Errorf("%s tagged its request %q, which Operations has no entry for", name, id)
			continue
		}
		reached[id] = true
		if got.Method != op.Method || !pathFits(op.Path, got.URL.EscapedPath()) {
			t.Errorf("%s sent %s %s tagged %q, and Operations has %s %s",
				name, got.Method, got.URL.EscapedPath(), id, op.Method, op.Path)
		}
		if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(id)) {
			t.Errorf("%s tagged its request %q, another operation's id", name, id)
		}
	}
	for id := range Operations {
		if !reached[id] {
			t.Errorf("no client method sends %s", id)
		}
	}
}

// placeholderArgs fills a generated method's arguments: a context, a distinct
// lowercase word for every string-kinded path parameter, and the zero value
// for the rest. The variadic request editors are left empty.
func placeholderArgs(fn reflect.Type) []reflect.Value {
	args := []reflect.Value{reflect.ValueOf(context.Background())}
	last := fn.NumIn()
	if fn.IsVariadic() {
		last--
	}
	for i := 1; i < last; i++ {
		t := fn.In(i)
		if t.Kind() == reflect.String {
			args = append(args, reflect.ValueOf("p"+string(rune('a'+i))).Convert(t))
		} else {
			args = append(args, reflect.Zero(t))
		}
	}
	return args
}

// patternFillers are values shaped like the id patterns the contract declares
// that a lowercase word doesn't match, tried in order.
var patternFillers = []string{"inst-" + strings.Repeat("0", 32), "inv-0"}

// fitRefusedArg swaps the argument a path parameter refusal names for a value
// its rule takes: the first value an enum lists, or the next pattern filler.
// It is false when there is nothing left to try.
func fitRefusedArg(args []reflect.Value, pe *PathParamError) bool {
	for i, a := range args {
		if a.Kind() != reflect.String || a.String() != pe.Value {
			continue
		}
		var next string
		switch pe.Rule {
		case "enum":
			_, values, _ := strings.Cut(pe.Reason, ": ")
			next, _, _ = strings.Cut(values, ", ")
		case "pattern":
			j := slices.Index(patternFillers, pe.Value) + 1
			if j == len(patternFillers) {
				return false
			}
			next = patternFillers[j]
		default:
			return false
		}
		if next == "" {
			return false
		}
		args[i] = reflect.ValueOf(next).Convert(a.Type())
		return true
	}
	return false
}

// pathFits is whether path is template with each {parameter} filled by one
// non-empty segment.
func pathFits(template, path string) bool {
	want, got := strings.Split(template, "/"), strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i, w := range want {
		if strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}") {
			if got[i] == "" {
				return false
			}
		} else if w != got[i] {
			return false
		}
	}
	return true
}

// The table says what the prepared spec says. The spec is read here
// independently of the generator, by walking its paths, so an entry the
// template renders wrong, or an operation it skips, fails.
func TestOperationsMatchThePreparedSpec(t *testing.T) {
	raw, err := os.ReadFile("spec/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	type response struct {
		Ref         string `yaml:"$ref"`
		Description string `yaml:"description"`
	}
	type operation struct {
		OperationID string              `yaml:"operationId"`
		Deprecated  bool                `yaml:"deprecated"`
		Responses   map[string]response `yaml:"responses"`
	}
	var spec struct {
		Paths      map[string]map[string]yaml.Node `yaml:"paths"`
		Components struct {
			Responses map[string]response `yaml:"responses"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}

	inSpec := map[string]bool{}
	for path, item := range spec.Paths {
		for method, node := range item {
			switch method {
			case "get", "put", "post", "delete", "patch", "head", "options", "trace":
			default:
				continue
			}
			var op operation
			if err := node.Decode(&op); err != nil {
				t.Fatalf("decode %s %s: %v", method, path, err)
			}
			inSpec[op.OperationID] = true
			got, ok := Operations[op.OperationID]
			if !ok {
				t.Errorf("%s %s (%s) is in the spec and not in Operations", method, path, op.OperationID)
				continue
			}
			if got.Method != strings.ToUpper(method) || got.Path != path || got.Deprecated != op.Deprecated {
				t.Errorf("%s: Operations has %s %s deprecated=%v, the spec %s %s deprecated=%v",
					op.OperationID, got.Method, got.Path, got.Deprecated, strings.ToUpper(method), path, op.Deprecated)
			}
			want := map[int]ResponseInfo{}
			for status, r := range op.Responses {
				code, err := strconv.Atoi(status)
				if err != nil {
					continue // "default" or a range like "4XX": the table keys exact statuses only.
				}
				desc := r.Description
				if r.Ref != "" {
					desc = spec.Components.Responses[strings.TrimPrefix(r.Ref, "#/components/responses/")].Description
				}
				if code/100 == 2 {
					desc = ""
				}
				want[code] = ResponseInfo{Description: desc, Ref: r.Ref}
			}
			if !reflect.DeepEqual(got.Responses, want) {
				t.Errorf("%s: Operations has responses\n%+v\nthe spec\n%+v", op.OperationID, got.Responses, want)
			}
		}
	}
	for id := range Operations {
		if !inSpec[id] {
			t.Errorf("Operations has %s, which the spec doesn't", id)
		}
	}
}
