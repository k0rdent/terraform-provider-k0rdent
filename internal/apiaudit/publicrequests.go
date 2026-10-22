// Package apiaudit checks that every request the provider sent was one a
// tenant may send.
package apiaudit

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The provider is tenant-facing, and the generated client keeps only the
// operations the contract marks public, so a request built through it can't
// reach an operator-only one. That's a property of how the code is meant to be
// written. This reads what actually went over the wire: every request the fake
// logged is matched to the contract operation it called, and any that isn't
// public is named. The only requests left out are the harness's own, which
// carry HarnessUserAgent. Everything else counts as the provider, including a
// request built by hand that went out as Go-http-client, because that's the
// one a generated-client guarantee says nothing about.
//
// It covers the requests a run drove, and nothing else. A code path no test
// exercises sends nothing here, so it proves nothing about that path.

// Route is where an operation lives: its method and its path template, as the
// contract spells them.
type Route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type compiledRoute struct {
	op       string
	segments []string
	literals int
}

// routesByMethod is APIRoutes split into segments once, grouped by method.
var routesByMethod = compileRoutes(APIRoutes)

// compileRoutes groups routes by method, each group sorted by operationId so a
// tie between equally literal routes resolves the same way every run.
func compileRoutes(routes map[string]Route) map[string][]compiledRoute {
	out := map[string][]compiledRoute{}
	for op, r := range routes {
		segs := strings.Split(strings.Trim(r.Path, "/"), "/")
		literals := 0
		for _, s := range segs {
			if !isParam(s) {
				literals++
			}
		}
		out[r.Method] = append(out[r.Method], compiledRoute{op: op, segments: segs, literals: literals})
	}
	for _, group := range out {
		sort.Slice(group, func(i, j int) bool { return group[i].op < group[j].op })
	}
	return out
}

func isParam(seg string) bool { return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") }

// OperationFor names the contract operation a request called. A `{param}`
// matches exactly one non-empty segment, and a query string is ignored. When a
// literal route and a templated one both match, as `/v1/regions/global/...`
// can match `/v1/regions/{region}/...`, the one with more literal segments
// wins, which is what a router does too. Between two equally literal routes
// the first operationId in sort order wins.
func OperationFor(method, path string) (string, bool) {
	return operationFor(routesByMethod, method, path)
}

func operationFor(routes map[string][]compiledRoute, method, path string) (string, bool) {
	path, _, _ = strings.Cut(path, "?")
	segs := strings.Split(strings.Trim(path, "/"), "/")
	best, bestLiterals := "", -1
	for _, r := range routes[method] {
		if r.literals > bestLiterals && segmentsMatch(r.segments, segs) {
			best, bestLiterals = r.op, r.literals
		}
	}
	return best, best != ""
}

func segmentsMatch(template, segs []string) bool {
	if len(template) != len(segs) {
		return false
	}
	for i, t := range template {
		switch {
		case segs[i] == "":
			return false
		case isParam(t):
		case t != segs[i]:
			return false
		}
	}
	return true
}

// HarnessUserAgent is what the acceptance harness sends when it calls the
// fake itself, for setup, drift or a fake-only route. It's the one thing the audit leaves out, so it names who's calling and
// not what the provider calls itself.
const HarnessUserAgent = "k0rdent-test-harness"

// fakeLogRequest reads `<date> <time> METHOD /path ua="…"[ auth=present]`, the
// whole line, so a line that's only nearly that shape isn't half read.
var fakeLogRequest = regexp.MustCompile(
	`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} ([A-Z]+) (/\S*) ua=("(?:[^"\\]|\\.)*")(?: auth=present)?$`)

// isRequestLine is whether the fake wrote this line for a request. Every
// request line carries ` ua=`, and the fake's own messages don't.
func isRequestLine(line string) bool { return strings.Contains(line, " ua=") }

// RequestAudit is what AuditProviderRequests found.
type RequestAudit struct {
	// Requests is how many request lines the audit checked, which is every
	// one but the harness's.
	Requests int
	// Harness is how many request lines carried HarnessUserAgent and were
	// left out.
	Harness int
	// Public is every distinct public operation they called, sorted.
	Public []string
	// Refused names each request that wasn't a public operation, in log order.
	Refused []string
}

// AuditProviderRequests reads the fake's request log and checks every request
// in it against the contract, except the ones the harness sent as itself.
//
// Nothing else is exempt. A request line it can't read is refused rather than
// dropped, since a line it can't read is one it can't show is public. The token mint is in the contract as mintToken and marked
// public, so it's checked like any other request. The fake's own `/_demo/` and
// `/_harness/` routes aren't in the contract at all, so if the provider ever
// sent one it would be refused as matching nothing, which is right: the
// provider has no business there.
func AuditProviderRequests(log string) RequestAudit {
	var a RequestAudit
	public := map[string]bool{}
	for _, line := range strings.Split(log, "\n") {
		if !isRequestLine(line) {
			continue
		}
		m := fakeLogRequest.FindStringSubmatch(line)
		if m == nil {
			a.Requests++
			a.Refused = append(a.Refused, fmt.Sprintf("can't read this request line, so it can't be shown public: %q", line))
			continue
		}
		if ua, err := strconv.Unquote(m[3]); err == nil && ua == HarnessUserAgent {
			a.Harness++
			continue
		}
		a.Requests++
		method, path := m[1], m[2]
		op, ok := OperationFor(method, path)
		switch {
		case !ok:
			a.Refused = append(a.Refused, fmt.Sprintf("%s %s matches no operation in the contract", method, path))
		case !isPublic(op):
			v, _ := visibilityOf(op)
			a.Refused = append(a.Refused, fmt.Sprintf("%s %s is %s, which the contract marks %s",
				method, path, op, strings.Join(v, ", ")))
		default:
			public[op] = true
		}
	}
	for op := range public {
		a.Public = append(a.Public, op)
	}
	sort.Strings(a.Public)
	return a
}

// Err is nil only when the provider sent at least one request and every one
// was public. A log with no provider request in it proves nothing, so it isn't
// a pass.
func (a RequestAudit) Err() error {
	if len(a.Refused) > 0 {
		return fmt.Errorf("%d of %d requests were not a public operation:\n  %s",
			len(a.Refused), a.Requests, strings.Join(a.Refused, "\n  "))
	}
	if a.Requests == 0 {
		return fmt.Errorf("the log has no request but the harness's own (%d of those), so this proves nothing", a.Harness)
	}
	return nil
}

// String is the one-line summary a passing run prints.
func (a RequestAudit) String() string {
	return fmt.Sprintf("%d requests, all public, across %d operations (%d harness requests left out)",
		a.Requests, len(a.Public), a.Harness)
}
