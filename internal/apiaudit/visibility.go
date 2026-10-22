package apiaudit

import (
	_ "embed"
	"encoding/json"
	"strings"
)

// "The platform cannot do this" and "the platform can do this and a tenant may
// not ask" look identical from inside the provider: the generated client is
// built for the tenant plane, so an operator-only operation is simply not in
// it. The contract records which is which per operation as `x-visibility`.
// That field survives nowhere downstream, so it is captured from the spec into
// data/visibility.json by scripts/gen-api-visibility.py and embedded here.

//go:embed data/visibility.json
var visibilityJSON []byte

type visibilityFile struct {
	Source     VisibilitySpec      `json:"source"`
	Operations map[string][]string `json:"operations"`
	Routes     map[string]Route    `json:"routes"`
}

// VisibilitySpec identifies the spec data/visibility.json was generated from.
type VisibilitySpec struct {
	ReleaseTag string `json:"releaseTag"`
	SHA256     string `json:"sha256"`
}

var visibility = func() visibilityFile {
	var f visibilityFile
	if err := json.Unmarshal(visibilityJSON, &f); err != nil {
		panic("apiaudit: data/visibility.json is unreadable: " + err.Error())
	}
	return f
}()

// APIVisibility maps an operationId to the planes that may call it.
var APIVisibility = visibility.Operations

// APIRoutes maps an operationId to its method and path template.
var APIRoutes = visibility.Routes

// VisibilitySource is the spec APIVisibility was read from.
var VisibilitySource = visibility.Source

// visibilityOf normalises the contract's operationId casing. The generated Go
// client exports CreateCluster where the contract says createCluster.
func visibilityOf(op string) ([]string, bool) {
	if v, ok := APIVisibility[op]; ok {
		return v, true
	}
	lower := strings.ToLower(op[:1]) + op[1:]
	v, ok := APIVisibility[lower]
	return v, ok
}

func isPublic(op string) bool {
	v, ok := visibilityOf(op)
	if !ok {
		return false
	}
	for _, p := range v {
		if p == "public" {
			return true
		}
	}
	return false
}
