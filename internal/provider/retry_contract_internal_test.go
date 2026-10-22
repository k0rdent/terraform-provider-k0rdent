package provider

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// genericConflict is the contract's shared 409.
const genericConflict = "#/components/responses/Conflict"

// regionMismatchConflict is the 409 v1.3.0-dev.4 puts on project-scoped routes
// for a project locked to another region, and regionMismatchMeans is the part
// of its description that decides it: no retry can clear it, so no operation
// that carries it is resent or waited out. regionMismatch.go turns it into a
// diagnostic that names the project's region instead.
const (
	regionMismatchConflict = "#/components/responses/ProjectRegionMismatch"
	regionMismatchMeans    = "retrying the same URL returns the same result"
)

// genericUnavailable is the contract's shared 503, and what it means is why an
// operation that carries it isn't resent.
const (
	genericUnavailable      = "#/components/responses/ServiceUnavailable"
	genericUnavailableMeans = "because a required dependency is not configured"
)

// contractOperation is what the 503 decision needs from one operation.
type contractOperation struct {
	method, path string
	// own503 is the operation's own 503 description, or "" when it has none
	// or shares the generic one.
	own503 string
	// own409 is the operation's own 409 description, or "" when it has none
	// or shares the generic one.
	own409 string
	// shared409 is the contract's shared Conflict description, for an
	// operation whose 409 is that $ref.
	shared409 string
	// regionMismatch is set for an operation whose 409 is the shared
	// ProjectRegionMismatch response and nothing else.
	regionMismatch bool
}

// pinnedContract reads the operations the generated client was built from,
// out of its own table, and the contract's generic 503 description.
func pinnedContract(t *testing.T) (map[string]contractOperation, string) {
	t.Helper()
	ops := map[string]contractOperation{}
	var generic string
	for id, op := range k0rdent.Operations {
		got := contractOperation{method: op.Method, path: op.Path}
		if r, ok := op.Responses[http.StatusServiceUnavailable]; ok {
			switch r.Ref {
			case "":
				got.own503 = r.Description
			case genericUnavailable:
				generic = r.Description
			default:
				t.Errorf("%s answers 503 with %s, which this test doesn't know the meaning of", id, r.Ref)
			}
		}
		switch r, ok := op.Responses[http.StatusConflict]; {
		case !ok:
		case r.Ref == "":
			got.own409 = r.Description
		case r.Ref == genericConflict:
			got.shared409 = r.Description
		case r.Ref == regionMismatchConflict:
			got.regionMismatch = true
			if !strings.Contains(foldSpace.ReplaceAllString(r.Description, " "), regionMismatchMeans) {
				t.Errorf("the contract's ProjectRegionMismatch no longer says %q, so the operations that "+
					"carry it need deciding again. It says:\n%s", regionMismatchMeans, r.Description)
			}
		default:
			t.Errorf("%s answers 409 with %s, which this test doesn't know the meaning of", id, r.Ref)
		}
		ops[id] = got
	}
	return ops, generic
}

var foldSpace = regexp.MustCompile(`\s+`)

// The list of POSTs and PATCHes whose 503 is waited out is prose turned into
// code, and the contract carries no mark that could generate it. So every
// direction it could drift in fails here instead.
func TestEveryPostAndPatch503IsDecided(t *testing.T) {
	ops, generic := pinnedContract(t)
	if len(ops) == 0 {
		t.Fatal("the spec has no operations")
	}

	// The shared 503 is reported at once because of what it means. If the
	// contract changes that meaning, every operation that carries it needs
	// deciding again.
	if !strings.Contains(foldSpace.ReplaceAllString(generic, " "), genericUnavailableMeans) {
		t.Errorf("the contract's generic 503 no longer says %q, so the operations that carry it "+
			"need deciding again. It says:\n%s", genericUnavailableMeans, generic)
	}

	for id, op := range ops {
		if op.method != http.MethodPost && op.method != http.MethodPatch || op.own503 == "" {
			continue
		}
		if _, ok := post503Decisions[id]; !ok {
			t.Errorf("%s %s (%s) describes a 503 of its own and post503Decisions has no decision on it. "+
				"Read its description, then add it as resent or refused, with the sentence that decides it:\n%s",
				op.method, op.path, id, op.own503)
		}
	}

	for id, d := range post503Decisions {
		op, ok := ops[id]
		switch {
		case !ok:
			t.Errorf("post503Decisions decides %s, which the contract no longer has", id)
			continue
		case op.method != http.MethodPost && op.method != http.MethodPatch:
			t.Errorf("post503Decisions decides %s, which is a %s: the method rule decides those", id, op.method)
		case op.own503 == "":
			t.Errorf("post503Decisions decides %s, which no longer describes a 503 of its own", id)
		case !strings.Contains(foldSpace.ReplaceAllString(op.own503, " "), d.says):
			t.Errorf("%s's 503 no longer says %q, which is what its decision stands on. It says:\n%s",
				id, d.says, op.own503)
		}
		if d.resends == (d.refused != "") {
			t.Errorf("%s: a decision either resends or says why it refuses, and this one does %s",
				id, map[bool]string{true: "both", false: "neither"}[d.resends])
		}
	}
}
