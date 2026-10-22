package provider

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// A configured provider keeps its connections when something else in the
// process closes the shared default pool's idle ones, because they were never
// in it. Before each provider had a pool of its own, the second read below
// had to dial again, and a request racing that close failed outright.
func TestAProvidersConnectionsSurviveTheSharedPoolClosing(t *testing.T) {
	var opened atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		answer := `{"id":"c1","state":"active"}`
		if strings.HasSuffix(r.URL.Path, "/auth/token") {
			answer = `{"access_token":"pool-test","token_type":"Bearer","expires_in":3600}`
		}
		if _, err := io.WriteString(w, answer); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	meta, err := newMeta(Settings{Endpoint: server.URL, Credential: CredentialSource{Kind: APIKeyCredential, APIKey: "k"}}, "test")
	if err != nil {
		t.Fatalf("new meta: %v", err)
	}
	read := func() {
		t.Helper()
		got, err := meta.Client.GetClusterWithResponse(context.Background(), "test", "test", "c1", nil)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got.StatusCode() != http.StatusOK {
			t.Fatalf("read: status %d", got.StatusCode())
		}
	}

	read()
	before := opened.Load()
	// What httptest.Server.Close does to the shared pool on every call.
	if shared, ok := http.DefaultTransport.(*http.Transport); ok {
		shared.CloseIdleConnections()
	}
	read()
	if after := opened.Load(); after != before {
		t.Errorf("the provider dialled %d new connection(s) after the shared pool closed its idle ones, "+
			"so its connections were in that pool", after-before)
	}
}

// The provider's pool is a copy of Go's default, not a tuned one: proxy
// handling, dial and handshake timeouts and keep-alive limits are what they
// were when the provider sent through http.DefaultTransport itself.
func TestThePlatformTransportKeepsGoDefaults(t *testing.T) {
	shared, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport is not an *http.Transport in this process")
	}
	own := NewPlatformTransport()
	if own == shared {
		t.Fatal("NewPlatformTransport returned the shared default itself")
	}
	if own == NewPlatformTransport() {
		t.Error("two calls returned the same pool")
	}
	sameAsGoDefaults(t, own, shared)
}

// sameAsGoDefaults checks own carries every setting of Go's default
// transport that can be compared. TLSClientConfig isn't one: net/http fills
// it in on a transport's first request, so whether it's set says
// what the process has sent, not how the pool was built.
func sameAsGoDefaults(t *testing.T, own, shared *http.Transport) {
	t.Helper()
	if own.Proxy == nil || own.DialContext == nil {
		t.Error("the pool lost proxy handling or the default dialer")
	}
	for name, pair := range map[string][2]any{
		"ForceAttemptHTTP2":     {own.ForceAttemptHTTP2, shared.ForceAttemptHTTP2},
		"MaxIdleConns":          {own.MaxIdleConns, shared.MaxIdleConns},
		"MaxIdleConnsPerHost":   {own.MaxIdleConnsPerHost, shared.MaxIdleConnsPerHost},
		"MaxConnsPerHost":       {own.MaxConnsPerHost, shared.MaxConnsPerHost},
		"IdleConnTimeout":       {own.IdleConnTimeout, shared.IdleConnTimeout},
		"TLSHandshakeTimeout":   {own.TLSHandshakeTimeout, shared.TLSHandshakeTimeout},
		"ExpectContinueTimeout": {own.ExpectContinueTimeout, shared.ExpectContinueTimeout},
		"ResponseHeaderTimeout": {own.ResponseHeaderTimeout, shared.ResponseHeaderTimeout},
		"DisableKeepAlives":     {own.DisableKeepAlives, shared.DisableKeepAlives},
		"DisableCompression":    {own.DisableCompression, shared.DisableCompression},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %v, want Go's default %v", name, pair[0], pair[1])
		}
	}
}

// Every client in this repository, the provider's and every test's, sends
// through a connection pool of its own. http.DefaultTransport is the one pool
// the whole process shares, and anything holding it can close its idle
// connections under a request that has just picked one up: httptest.Server.Close
// does exactly that, and a DELETE caught that way fails with "http:
// CloseIdleConnections called" rather than being replayed. That made a unit
// test fail at random in `make test`. This test fails on every way of
// reaching the shared pool that TestTheSharedTransportGuardSeesEverySpelling
// lists, in production code and tests alike, so the fix doesn't quietly come
// undone.

// The only functions allowed to name http.DefaultTransport. NewPlatformTransport
// and the client's ownPool clone it and never send through it, and the rest
// are the tests that check a pool is its own and matches Go's defaults.
var sharedTransportAllowed = map[string]bool{
	"internal/k0rdent/client.go:ownPool":                                                                       true,
	"internal/k0rdent/client_test.go:TestNew_SurvivesTheSharedPoolClosing":                                     true,
	"internal/provider/shared_transport_internal_test.go:TestTheFallbackTransportIsGoDefaults":                 true,
	"internal/provider/transport.go:NewPlatformTransport":                                                      true,
	"internal/provider/shared_transport_internal_test.go:TestAProvidersConnectionsSurviveTheSharedPoolClosing": true,
	"internal/provider/shared_transport_internal_test.go:TestThePlatformTransportKeepsGoDefaults":              true,
}

const k0rdentClientImport = "github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"

func TestNothingSendsThroughTheSharedTransport(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// The generated client's fallback http.Client is only reached by a
		// caller that passes none, and the guard below sees every caller.
		if generatedClientFile(filepath.ToSlash(rel)) {
			return nil
		}
		found, err := sharedTransportUses(path, filepath.ToSlash(rel), nil)
		offenders = append(offenders, found...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("these reach the process-wide http.DefaultTransport, which another "+
			"caller's CloseIdleConnections can break a request on. Give the client a "+
			"pool of its own (NewPlatformTransport, or testTransport in a test):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// sharedTransportUses lists each place in one file that sends, or lets a
// client send, through http.DefaultTransport.
func sharedTransportUses(path, rel string, src any) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	httpName, k0rdentName := "", ""
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		name := filepath.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch p {
		case "net/http":
			httpName = name
		case k0rdentClientImport:
			k0rdentName = name
		}
	}
	if httpName == "" && k0rdentName == "" {
		return nil, nil
	}

	var out []string
	report := func(n ast.Node, fn, why string) {
		if why == "names http.DefaultTransport" && sharedTransportAllowed[rel+":"+fn] {
			return
		}
		out = append(out, rel+":"+strconv.Itoa(fset.Position(n.Pos()).Line)+": "+why)
	}
	is := func(e ast.Expr, pkg, name string) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || pkg == "" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == pkg && sel.Sel.Name == name
	}

	for _, decl := range file.Decls {
		fn := ""
		if f, ok := decl.(*ast.FuncDecl); ok {
			fn = f.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				switch {
				case is(n, httpName, "DefaultTransport"):
					report(n, fn, "names http.DefaultTransport")
				case is(n, httpName, "DefaultClient"):
					report(n, fn, "names http.DefaultClient, which sends through http.DefaultTransport")
				case is(n, httpName, "Get"), is(n, httpName, "Head"), is(n, httpName, "Post"), is(n, httpName, "PostForm"):
					report(n, fn, "calls http."+n.Sel.Name+", which sends through http.DefaultTransport")
				}
			case *ast.CompositeLit:
				if is(n.Type, httpName, "Client") && !setsTransport(n) {
					report(n, fn, noTransport)
				}
			case *ast.ValueSpec:
				if is(n.Type, httpName, "Client") && len(n.Values) == 0 {
					report(n, fn, noTransport)
				}
			case *ast.CallExpr:
				switch {
				case isBuiltinNew(n) && is(n.Args[0], httpName, "Client"):
					report(n, fn, noTransport)
				case (is(n.Fun, k0rdentName, "NewClient") || is(n.Fun, k0rdentName, "NewClientWithResponses")) &&
					!passesOption(n, k0rdentName, "WithHTTPClient"):
					report(n, fn, "builds a generated k0rdent client without k0rdent.WithHTTPClient, which falls back to http.DefaultTransport")
				}
			}
			return true
		})
	}
	return out, nil
}

// noTransport is why an http.Client built with no Transport is flagged.
const noTransport = "builds an http.Client with no Transport, which falls back to http.DefaultTransport"

// setsTransport says whether a client literal sets Transport to something
// other than nil.
func setsTransport(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Transport" {
			v, isIdent := kv.Value.(*ast.Ident)
			return !isIdent || v.Name != "nil"
		}
	}
	return false
}

func isBuiltinNew(call *ast.CallExpr) bool {
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "new" && len(call.Args) == 1
}

// passesOption says whether a call passes k0rdent.<option>(...) as one of its
// own arguments.
func passesOption(call *ast.CallExpr, k0rdentName, option string) bool {
	for _, arg := range call.Args {
		inner, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := inner.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == option {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == k0rdentName {
				return true
			}
		}
	}
	return false
}

// A wrapper built without the transport under it refuses the request with an
// error that says so, rather than panicking mid-apply or quietly falling back
// to the shared pool.
func TestAWrapperWithNoBaseRefusesClearly(t *testing.T) {
	for name, rt := range map[string]http.RoundTripper{
		"loggingTransport":           loggingTransport{},
		"rateLimitTransport":         rateLimitTransport{},
		"newRateLimitTransport(nil)": newRateLimitTransport(nil),
	} {
		req, err := http.NewRequest(http.MethodGet, "http://unused.invalid/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := rt.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "no transport under it") {
			t.Errorf("%s: err = %v, want one naming the missing transport", name, err)
		}
	}
}

// The fallback for a process whose default transport isn't an
// *http.Transport is Go's default spelled out, so it matches the clone.
func TestTheFallbackTransportIsGoDefaults(t *testing.T) {
	shared, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport is not an *http.Transport in this process")
	}
	sameAsGoDefaults(t, goDefaultTransport(), shared)
}

// Each way of reaching the shared pool the guard knows, spelled the ways Go
// allows, and the spellings that don't reach it.
func TestTheSharedTransportGuardSeesEverySpelling(t *testing.T) {
	const src = `package x

import (
	nh "net/http"

	kc "github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var own = nh.Transport{}

func reaches() {
	_ = &nh.Client{}                               // line 12
	_ = nh.Client{Transport: nil}                  // line 13
	_ = new(nh.Client)                             // line 14
	var c nh.Client                                // line 15
	_ = c
	_, _ = kc.NewClient("http://x")                // line 17
	_, _ = kc.NewClientWithResponses("http://x")   // line 18
	_ = nh.DefaultTransport                        // line 19
	_ = nh.DefaultClient                           // line 20
	_, _ = nh.Get("http://x")                      // line 21
}

func keepsItsOwn() {
	_ = &nh.Client{Transport: &own}
	var c = nh.Client{Transport: &own}
	_ = c
	_, _ = kc.New("http://x") // New gives each client a pool of its own
	_, _ = kc.NewClient("http://x", kc.WithHTTPClient(&nh.Client{Transport: &own}))
	_, _ = kc.NewClientWithResponses("http://x", kc.WithHTTPClient(&nh.Client{Transport: &own}))
}
`
	got, err := sharedTransportUses("x.go", "x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, o := range got {
		lines = append(lines, strings.SplitN(o, ":", 3)[1])
	}
	want := []string{"12", "13", "14", "15", "17", "18", "19", "20", "21"}
	if strings.Join(lines, " ") != strings.Join(want, " ") {
		t.Errorf("flagged lines %v, want %v:\n  %s", lines, want, strings.Join(got, "\n  "))
	}
}
