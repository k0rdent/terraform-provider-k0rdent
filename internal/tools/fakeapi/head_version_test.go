package main

import (
	"net/http"
	"strings"
	"testing"
)

// The contract gives a ClusterType's head and an unversioned create two
// different versions: getClusterType "always resolves to the head's literal
// latest version, whatever its status", and creation "binds to the latest
// `available` version instead". gpu-training's newest row is a draft, so the
// two differ there, and a provider that took the head's versionId for the
// version a cluster binds would be wrong against the real API too.
func TestClusterTypeHeadIsTheNewestRowNotTheVersionACreateBinds(t *testing.T) {
	s := newState()
	code, head := call(t, s.getClusterType, http.MethodGet, "gpu-training", "")
	if code != http.StatusOK {
		t.Fatalf("getClusterType: %d %v", code, head)
	}
	if head["versionId"] != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("the head reports versionId %v, and the newest row is the v4 draft", head["versionId"])
	}
	resolved, _, _, ok := resolveClusterTypeRef("r", clusterTypePath("r", "gpu-training"))
	if !ok {
		t.Fatal("the head reference didn't resolve")
	}
	if _, bound, _ := strings.Cut(resolved, "/versions/"); bound != "33333333-3333-3333-3333-333333333333" {
		t.Fatalf("an unversioned create bound %s, and the newest available version is v3", bound)
	}
}
