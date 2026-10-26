package k0rdent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const testServer = "https://api.example.test"

var testInstance = "inst-" + strings.Repeat("0a", 16)

// Each case goes through a real generated builder. The dot-segment cases are
// the ones that used to build a URL for another route: RFC 3986 resolution
// against the server address removes `.` and `..`, so before the check id
// ".." on deleteCluster built DELETE /v1/regions/r1/projects/proj/compute/.
func TestBuildersRefuseAPathParameterBeforeBuildingTheURL(t *testing.T) {
	for _, tc := range []struct {
		name      string
		build     func() (*http.Request, error)
		operation string
		param     string
		value     string
		rule      string
	}{
		{"dot-dot id", func() (*http.Request, error) { return NewDeleteClusterRequest(testServer, "r1", "proj", "..") },
			"deleteCluster", "id", "..", "segment"},
		{"dot id", func() (*http.Request, error) { return NewDeleteClusterRequest(testServer, "r1", "proj", ".") },
			"deleteCluster", "id", ".", "segment"},
		{"empty id", func() (*http.Request, error) { return NewDeleteClusterRequest(testServer, "r1", "proj", "") },
			"deleteCluster", "id", "", "segment"},
		{"dot-dot region", func() (*http.Request, error) { return NewGetClusterRequest(testServer, "..", "proj", "c1", nil) },
			"getCluster", "region", "..", "segment"},
		{"empty project", func() (*http.Request, error) { return NewGetClusterRequest(testServer, "r1", "", "c1", nil) },
			"getCluster", "project", "", "segment"},
		{"dot-dot in a builder with a body", func() (*http.Request, error) {
			return NewUpdateClusterRequest(testServer, "r1", "..", "c1", UpdateClusterJSONRequestBody{})
		}, "updateCluster", "project", "..", "segment"},
		{"organization id off the contract's pattern", func() (*http.Request, error) { return NewGetOrganizationRequest(testServer, "Org_1") },
			"getOrganization", "id", "Org_1", "pattern"},
		{"organization id over the contract's maxLength", func() (*http.Request, error) {
			return NewGetOrganizationRequest(testServer, "a"+strings.Repeat("b", 63))
		}, "getOrganization", "id", "a" + strings.Repeat("b", 63), "maxLength"},
		{"instance id off the contract's pattern", func() (*http.Request, error) { return NewGetInstanceRequest(testServer, "r1", "proj", "inst-abc", nil) },
			"getInstance", "id", "inst-abc", "pattern"},
		{"power action the enum doesn't name", func() (*http.Request, error) {
			return NewExecuteInstancePowerActionRequest(testServer, "r1", "proj", testInstance, "reboot")
		}, "executeInstancePowerAction", "action", "reboot", "enum"},
		{"power action in the wrong case", func() (*http.Request, error) {
			return NewExecuteInstancePowerActionRequest(testServer, "r1", "proj", testInstance, "PowerOn")
		}, "executeInstancePowerAction", "action", "PowerOn", "enum"},
		{"dot-dot where the contract has a pattern too", func() (*http.Request, error) { return NewGetInstanceRequest(testServer, "r1", "proj", "..", nil) },
			"getInstance", "id", "..", "segment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := tc.build()
			if req != nil {
				t.Errorf("built %s %s, want no request", req.Method, req.URL.EscapedPath())
			}
			var pe *PathParamError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want a *PathParamError", err, err)
			}
			if pe.Operation != tc.operation || pe.Param != tc.param || pe.Value != tc.value || pe.Rule != tc.rule {
				t.Errorf("got {%s %s %q %s}, want {%s %s %q %s}",
					pe.Operation, pe.Param, pe.Value, pe.Rule, tc.operation, tc.param, tc.value, tc.rule)
			}
			if msg := err.Error(); !strings.Contains(msg, tc.operation) || !strings.Contains(msg, tc.param) {
				t.Errorf("message %q doesn't name the operation and the parameter", msg)
			}
		})
	}
}

// The check refuses only what changes which URL is built, and what the
// contract itself refuses. Everything else is escaped into its own segment,
// so `/`, `?`, `#`, `%` and `\` arrive inside it rather than editing the path.
// Refusing those would refuse a value the contract allows, such as a signed
// unsubscribe token, without making any URL safer.
func TestBuildersSendEveryOtherValueInsideItsOwnSegment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() (*http.Request, error)
		want  string
	}{
		{"identifiers", func() (*http.Request, error) { return NewDeleteClusterRequest(testServer, "r1", "proj", "c1") },
			"/v1/regions/r1/projects/proj/compute/clusters/c1"},
		{"a slash", func() (*http.Request, error) { return NewDeleteClusterRequest(testServer, "r1", "proj", "a/../b") },
			"/v1/regions/r1/projects/proj/compute/clusters/a%2F..%2Fb"},
		{"query and fragment marks", func() (*http.Request, error) { return NewDeleteClusterRequest(testServer, "r1", "proj", "a?b#c") },
			"/v1/regions/r1/projects/proj/compute/clusters/a%3Fb%23c"},
		{"a percent-encoded dot-segment", func() (*http.Request, error) { return NewDeleteClusterRequest(testServer, "r1", "proj", "%2e%2e") },
			"/v1/regions/r1/projects/proj/compute/clusters/%252e%252e"},
		{"a backslash", func() (*http.Request, error) { return NewDeleteClusterRequest(testServer, "r1", "proj", `a\b`) },
			"/v1/regions/r1/projects/proj/compute/clusters/a%5Cb"},
		{"dots that aren't a whole segment", func() (*http.Request, error) { return NewDeleteClusterRequest(testServer, "r1", "proj", "...") },
			"/v1/regions/r1/projects/proj/compute/clusters/..."},
		{"an organization id at the contract's maxLength", func() (*http.Request, error) {
			return NewGetOrganizationRequest(testServer, "a"+strings.Repeat("b", 62))
		}, "/v1/regions/global/organizations/a" + strings.Repeat("b", 62)},
		{"an instance id on the contract's pattern", func() (*http.Request, error) {
			return NewGetInstanceRequest(testServer, "r1", "proj", testInstance, nil)
		}, "/v1/regions/r1/projects/proj/compute/instances/" + testInstance},
		{"a power action the enum names", func() (*http.Request, error) {
			return NewExecuteInstancePowerActionRequest(testServer, "r1", "proj", testInstance, "gracefulRestart")
		}, "/v1/regions/r1/projects/proj/compute/instances/" + testInstance + "/gracefulRestart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := tc.build()
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got := req.URL.EscapedPath(); got != tc.want {
				t.Errorf("path = %s, want %s", got, tc.want)
			}
		})
	}
}

// Every operation the client has refuses a dot-segment in every string path
// parameter, and sends nothing. It is swept by reflection over the client's
// methods, so an operation the contract adds tomorrow is covered without
// anyone adding a case, and a template change that skips one builder fails
// here.
func TestEveryOperationRefusesADotSegmentAndSendsNothing(t *testing.T) {
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	reader := reflect.TypeFor[io.Reader]()
	methods := reflect.TypeFor[ClientInterface]()
	checked := 0
	for i := range methods.NumMethod() {
		m := methods.Method(i)
		call := reflect.ValueOf(c).MethodByName(m.Name)
		typ := call.Type()
		var segments []int
		typedBody := false
		for j := 1; j < typ.NumIn(); j++ {
			if typ.In(j).Kind() == reflect.Struct {
				typedBody = true
			}
			if typ.In(j).Kind() != reflect.String {
				continue
			}
			if j+1 < typ.NumIn() && typ.In(j+1) == reader {
				continue // the content type of a WithBody variant
			}
			segments = append(segments, j)
		}
		// A typed body is marshalled before its WithBody builder runs, and a
		// zero one can fail to marshal first. The WithBody variant is swept
		// with the same path parameters, and it is the builder that checks.
		if typedBody {
			continue
		}
		args := func(bad int, value string) []reflect.Value {
			out := []reflect.Value{reflect.ValueOf(context.Background())}
			for j := 1; j < typ.NumIn(); j++ {
				in := typ.In(j)
				if typ.IsVariadic() && j == typ.NumIn()-1 {
					break
				}
				switch {
				case j == bad:
					out = append(out, reflect.ValueOf(value).Convert(in))
				case in.Kind() == reflect.String:
					out = append(out, reflect.ValueOf("x").Convert(in))
				default:
					out = append(out, reflect.Zero(in))
				}
			}
			return out
		}
		send := func(in []reflect.Value) error {
			out := call.Call(in)
			if resp, _ := out[0].Interface().(*http.Response); resp != nil {
				_ = resp.Body.Close()
			}
			err, _ := out[1].Interface().(error)
			return err
		}
		// "x" fills nearly every parameter. Where the contract's own rule
		// refuses it, such as an instance id's pattern, the refusal could be
		// for the filler rather than for the dot segment, so only the error's
		// type is checked there. The table above covers those operations.
		var pe *PathParamError
		exact := !errors.As(send(args(0, "")), &pe)
		for _, bad := range segments {
			for _, value := range []string{"..", ".", ""} {
				before := sent.Load()
				err := send(args(bad, value))
				if sent.Load() != before {
					t.Errorf("%s with argument %d = %q sent a request", m.Name, bad, value)
				}
				switch {
				case !errors.As(err, &pe):
					t.Errorf("%s with argument %d = %q: err = %v, want a *PathParamError", m.Name, bad, value, err)
				case exact && (pe.Value != value || pe.Rule != "segment"):
					t.Errorf("%s with argument %d = %q: refused %s = %q by %s, want it refused as a segment",
						m.Name, bad, value, pe.Param, pe.Value, pe.Rule)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no operation had a string path parameter to check")
	}
}

// Two rules no parameter of today's contract reaches through a builder: every
// path parameter is escaped, and every minLength is 1, which the empty check
// already covers. The check is shared by every builder, so a contract that
// adds an unescaped parameter or a longer minimum gets both without a template
// change, and they are held here directly.
func TestCheckPathParamRulesNoBuilderReachesToday(t *testing.T) {
	for _, tc := range []struct {
		name           string
		value, segment string
		rule           pathParamRule
		want           string
	}{
		{"a raw slash in an unescaped segment", "a/b", "a/b", pathParamRule{}, "segment"},
		{"a raw query mark in an unescaped segment", "a?b", "a?b", pathParamRule{}, "segment"},
		{"shorter than a minLength over 1", "ab", "ab", pathParamRule{minLength: 3}, "minLength"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pe *PathParamError
			if err := checkPathParam("op", "p", tc.value, tc.segment, tc.rule); !errors.As(err, &pe) || pe.Rule != tc.want {
				t.Errorf("err = %v, want a *PathParamError by %s", err, tc.want)
			}
		})
	}
	// Multi-byte characters count once each, as JSON Schema counts length.
	if err := checkPathParam("op", "p", "ééé", "%C3%A9%C3%A9%C3%A9", pathParamRule{minLength: 3, maxLength: 3}); err != nil {
		t.Errorf("three characters refused by a length of 3: %v", err)
	}
}
