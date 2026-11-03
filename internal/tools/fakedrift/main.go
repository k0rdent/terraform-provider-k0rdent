// fakedrift checks that the fake answers in the same shape as the real API.
//
// The acceptance tests run against internal/tools/fakeapi, so a field the real
// API answers and the fake doesn't is a field no test ever reads. That is how
// an import of SSH users passed every test and failed on the real API: the
// fake echoed the keys it was sent, and the API answered references to them.
//
//	fakedrift record -upstream URL -listen ADDR -out FILE
//	    a reverse proxy to URL that appends one line per answer to FILE: the
//	    method, the path, the status, and the shape of the request and the
//	    response bodies. A shape is every field's path and JSON type, never a
//	    value, so no token, key or name lands in FILE. It prints its URL on
//	    the first line of stdout once it's listening.
//
//	fakedrift compare -spec openapi.yaml -fake FILE -real FILE [-accepted FILE]
//	    groups both recordings by operation (method, the spec's path template,
//	    status) and, for every operation both recorded, lists the fields only
//	    one side answered and the fields whose types differ. A line of the
//	    accepted file (JSON, see acceptedDiff) excuses one difference. Exit 1
//	    while any difference is unexcused.
//
// A field only one side answered can be data, not drift: an optional field the
// fake's fixtures never set. The accepted file says which, with a reason, so
// every difference is either fixed in the fake or written down.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fakedrift record|compare [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "record":
		err = record(os.Args[2:])
	case "compare":
		var clean bool
		clean, err = compare(os.Args[2:], os.Stdout)
		if err == nil && !clean {
			os.Exit(1)
		}
	default:
		err = fmt.Errorf("unknown command %q, want record or compare", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakedrift:", err)
		os.Exit(2)
	}
}

// exchange is one recorded answer.
type exchange struct {
	Method   string   `json:"method"`
	Path     string   `json:"path"`
	Status   int      `json:"status"`
	Request  []string `json:"request,omitempty"`
	Response []string `json:"response,omitempty"`
}

func record(args []string) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	upstream := fs.String("upstream", "", "the API to forward to")
	listen := fs.String("listen", "127.0.0.1:0", "where to listen")
	out := fs.String("out", "", "the recording to append to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *upstream == "" || *out == "" {
		return fmt.Errorf("record needs -upstream and -out")
	}
	target, err := url.Parse(*upstream)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var mu sync.Mutex
	write := func(e exchange) error {
		line, merr := json.Marshal(e)
		if merr != nil {
			return merr
		}
		mu.Lock()
		defer mu.Unlock()
		_, werr := f.Write(append(line, '\n'))
		return werr
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	fmt.Printf("http://%s\n", ln.Addr())
	return http.Serve(ln, recorder(target, write))
}

// recorder is a reverse proxy to target that hands write each answer's shape,
// beside the shape of the request that got it. An answer write can't record
// fails the request, so a run never compares a recording with holes in it.
func recorder(target *url.URL, write func(exchange) error) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		e := exchange{Method: resp.Request.Method, Path: resp.Request.URL.Path, Status: resp.StatusCode, Response: shapeOf(body)}
		if sent, ok := resp.Request.Context().Value(sentKey{}).([]byte); ok {
			e.Request = shapeOf(sent)
		}
		if err := write(e); err != nil {
			return fmt.Errorf("fakedrift could not record %s %s: %w", e.Method, e.Path, err)
		}
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The request body is read here, before the proxy sends it on, so its
		// shape can sit beside the answer's.
		if r.Body != nil {
			sent, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(sent))
			r = r.WithContext(contextWith(r, sent))
		}
		r.Host = target.Host
		proxy.ServeHTTP(w, r)
	})
}

// sentKey carries a request's body to the answer it gets.
type sentKey struct{}

func contextWith(r *http.Request, sent []byte) context.Context {
	return context.WithValue(r.Context(), sentKey{}, sent)
}

// shapeOf is every field of a JSON body as "path type", sorted, with an
// array's elements merged under "path[]". A body that isn't JSON has no shape.
func shapeOf(body []byte) []string {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	set := map[string]bool{}
	walk("$", v, set)
	return sorted(set)
}

func walk(path string, v any, set map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		set[path+" object"] = true
		for k, child := range x {
			walk(path+"."+k, child, set)
		}
	case []any:
		set[path+" array"] = true
		for _, child := range x {
			walk(path+"[]", child, set)
		}
	case string:
		set[path+" string"] = true
	case json.Number:
		set[path+" number"] = true
	case bool:
		set[path+" bool"] = true
	case nil:
		set[path+" null"] = true
	}
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// acceptedDiff excuses one difference: the operation, the field's path, and
// which side alone answers it ("fake", "real") or "type" for a type that
// differs. A field ending in "*" excuses every field under that prefix, for a
// subtree the fake leaves out on purpose. Reason says why it's data rather
// than drift. A line that excuses nothing fails the compare, so the file can't
// keep excuses for differences that are gone.
type acceptedDiff struct {
	Operation string `json:"operation"`
	Field     string `json:"field"`
	Side      string `json:"side"`
	Reason    string `json:"reason"`
	used      bool
}

func (a *acceptedDiff) excuses(op string, d difference) bool {
	if a.Operation != op || a.Side != d.side {
		return false
	}
	if prefix, ok := strings.CutSuffix(a.Field, "*"); ok {
		return strings.HasPrefix(d.field, prefix)
	}
	return a.Field == d.field
}

func compare(args []string, w io.Writer) (bool, error) {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	spec := fs.String("spec", "", "the OpenAPI spec whose path templates name the operations")
	fakeFile := fs.String("fake", "", "the fake's recording")
	realFile := fs.String("real", "", "the real API's recording")
	acceptedFile := fs.String("accepted", "", "the differences accepted as data, not drift")
	partial := fs.Bool("partial", false, "the recordings cover only some operations, so list the accepted lines they didn't show instead of failing on them")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	if *spec == "" || *fakeFile == "" || *realFile == "" {
		return false, fmt.Errorf("compare needs -spec, -fake and -real")
	}
	templates, err := pathTemplates(*spec)
	if err != nil {
		return false, err
	}
	fake, err := load(*fakeFile, templates)
	if err != nil {
		return false, err
	}
	real, err := load(*realFile, templates)
	if err != nil {
		return false, err
	}
	var accepted []acceptedDiff
	if *acceptedFile != "" {
		raw, err := os.ReadFile(*acceptedFile)
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(raw, &accepted); err != nil {
			return false, fmt.Errorf("%s: %w", *acceptedFile, err)
		}
		for _, a := range accepted {
			if a.Reason == "" {
				return false, fmt.Errorf("%s: %s %s has no reason", *acceptedFile, a.Operation, a.Field)
			}
		}
	}
	excusedBy := func(op string, d difference) bool {
		for i := range accepted {
			if accepted[i].excuses(op, d) {
				accepted[i].used = true
				return true
			}
		}
		return false
	}

	ops := map[string]bool{}
	for op := range fake {
		ops[op] = true
	}
	for op := range real {
		ops[op] = true
	}
	var compared, onlyFake, onlyReal []string
	unexcused, excused := 0, 0
	for _, op := range sorted(ops) {
		f, inFake := fake[op]
		r, inReal := real[op]
		switch {
		case !inReal:
			onlyFake = append(onlyFake, op)
			continue
		case !inFake:
			onlyReal = append(onlyReal, op)
			continue
		}
		compared = append(compared, op)
		for _, d := range diff(f, r) {
			if excusedBy(op, d) {
				excused++
				continue
			}
			unexcused++
			fmt.Fprintf(w, "%s\n  %s: %s\n", op, d.field, d.what)
		}
	}
	stale, unseen := 0, 0
	for _, a := range accepted {
		switch {
		case a.used:
		case *partial:
			unseen++
			fmt.Fprintf(w, "not seen in this partial run: %s %s (%s)\n", a.Operation, a.Field, a.Side)
		default:
			stale++
			fmt.Fprintf(w, "accepted, but no longer differs: %s %s (%s)\n", a.Operation, a.Field, a.Side)
		}
	}
	fmt.Fprintf(w, "\n%d operations compared, %d unexcused differences, %d accepted, %d stale accepted lines", len(compared), unexcused, excused, stale)
	if *partial {
		fmt.Fprintf(w, ", %d not seen", unseen)
	}
	fmt.Fprintln(w)
	if len(onlyReal) > 0 {
		fmt.Fprintf(w, "recorded on the real API only, so not compared:\n  %s\n", strings.Join(onlyReal, "\n  "))
	}
	if len(onlyFake) > 0 {
		fmt.Fprintf(w, "recorded on the fake only, so not compared:\n  %s\n", strings.Join(onlyFake, "\n  "))
	}
	return unexcused == 0 && stale == 0, nil
}

// fields is one operation's request and response fields, each with the JSON
// types seen at it.
type fields map[string]map[string]bool

type difference struct{ field, side, what string }

func diff(fake, real fields) []difference {
	all := map[string]bool{}
	for k := range fake {
		all[k] = true
	}
	for k := range real {
		all[k] = true
	}
	var out []difference
	for _, field := range sorted(all) {
		ft, rt := fake[field], real[field]
		switch {
		case rt == nil:
			out = append(out, difference{field, "fake", "the fake answers it, the real API never did"})
		case ft == nil:
			out = append(out, difference{field, "real", "the real API answers it, the fake never did"})
		default:
			if a, b := typesOf(ft), typesOf(rt); a != b {
				out = append(out, difference{field, "type", fmt.Sprintf("the fake answers %s, the real API %s", a, b)})
			}
		}
	}
	return out
}

func typesOf(t map[string]bool) string {
	return strings.Join(sorted(t), "|")
}

// load reads a recording into operation, then field, then types. A path no
// template matches, such as the fake's own /_harness calls, is left out.
func load(file string, templates []template) (map[string]fields, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string]fields{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e exchange
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		tmpl := match(templates, e.Path)
		if tmpl == "" {
			continue
		}
		op := fmt.Sprintf("%s %s %d", e.Method, tmpl, e.Status)
		if out[op] == nil {
			out[op] = fields{}
		}
		add := func(prefix string, shape []string) {
			for _, s := range shape {
				i := strings.LastIndexByte(s, ' ')
				field, typ := prefix+s[:i], s[i+1:]
				if out[op][field] == nil {
					out[op][field] = map[string]bool{}
				}
				out[op][field][typ] = true
			}
		}
		add("request ", e.Request)
		add("response ", e.Response)
	}
	return out, sc.Err()
}

type template struct {
	path string
	re   *regexp.Regexp
}

var param = regexp.MustCompile(`\\\{[^}]+\\\}`)

func pathTemplates(spec string) ([]template, error) {
	raw, err := os.ReadFile(spec)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Paths map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", spec, err)
	}
	var out []template
	for p := range doc.Paths {
		out = append(out, template{p, regexp.MustCompile("^" + param.ReplaceAllString(regexp.QuoteMeta(p), `[^/]+`) + "$")})
	}
	// The most literal template first, so a literal segment such as
	// /organizations/global isn't read as an organization's id.
	sort.Slice(out, func(i, j int) bool {
		ci, cj := strings.Count(out[i].path, "{"), strings.Count(out[j].path, "{")
		if ci != cj {
			return ci < cj
		}
		return out[i].path < out[j].path
	})
	return out, nil
}

func match(templates []template, path string) string {
	for _, t := range templates {
		if t.re.MatchString(path) {
			return t.path
		}
	}
	return ""
}
