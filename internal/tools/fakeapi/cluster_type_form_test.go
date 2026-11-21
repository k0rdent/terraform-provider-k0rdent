package main

import (
	"net/http"
	"strings"
	"testing"
)

// The platform's createCluster and createInstanceGroup parse the `clusterType`
// reference's form before they look the type up, and answer 422 "clusterType is
// not a valid ClusterType URI" for every malformed one. The fake
// refuses the same values first, so a form error is not answered as an
// unresolvable one.

func TestAMalformedClusterTypeIsRefusedOnBothCreates(t *testing.T) {
	s := newState()

	creates := map[string]struct {
		h    http.HandlerFunc
		body string
	}{
		"cluster":        {s.createCluster, `{"id":"c","clusterType":"%s"}`},
		"instance group": {s.createInstanceGroup, `{"id":"g","clusterType":"%s","nodePools":[{"nodePool":"workers","size":1}]}`},
	}

	// Every one of these the platform's parse refuses. The last is a form
	// resolveClusterTypeRef would have tidied into a valid one, so the guard
	// has to run before it for the refusal to be the platform's.
	refs := []string{
		"gpu-training",
		"regions/r/compute/cluster-types/gpu-training",
		"/v1/regions//compute/cluster-types/gpu-training",
		"/v1/regions/r/compute/cluster-types/",
		"/v1/regions/r/compute/cluster-types/gpu-training/",
		"/v1/regions/r/compute/cluster-types/gpu-training/versions/",
		"/v1/regions/r/compute/cluster-types/gpu-training/versions/v/extra",
		"/v1/regions/r/x/compute/cluster-types/gpu-training",
	}
	for name, create := range creates {
		for _, ref := range refs {
			code, out := in(t, create.h, http.MethodPost, "r", "p", nil, strings.Replace(create.body, "%s", ref, 1))
			if code != http.StatusUnprocessableEntity || errorMessage(out) != "validation: clusterType is not a valid ClusterType URI" {
				t.Errorf("%s with %q: %d %v", name, ref, code, out)
			}
		}
	}

	// A well-formed reference that names nothing is the other refusal, and
	// the form guard must not swallow it.
	for name, create := range creates {
		code, out := in(t, create.h, http.MethodPost, "r", "p", nil,
			strings.Replace(create.body, "%s", "/v1/regions/r/compute/cluster-types/absent", 1))
		if code != http.StatusUnprocessableEntity || !strings.Contains(errorMessage(out), "does not resolve to an available cluster type version") {
			t.Errorf("%s with an unresolvable reference: %d %v", name, code, out)
		}
	}

	// The platform parses the value as sent, so an absolute URL fails its
	// prefix even when its path is valid, and a valid path still creates.
	for name, create := range creates {
		ref := "https://api.example.invalid" + inferenceRef + "/versions/" + inferenceV2
		code, out := in(t, create.h, http.MethodPost, "r", "p", nil, strings.Replace(create.body, "%s", ref, 1))
		if code != http.StatusUnprocessableEntity || !strings.Contains(errorMessage(out), "clusterType is not a valid ClusterType URI") {
			t.Errorf("%s with an absolute reference: %d %v", name, code, out)
		}
		code, out = in(t, create.h, http.MethodPost, "r", "p", nil, strings.Replace(create.body, "%s", inferenceRef+"/versions/"+inferenceV2, 1))
		if code != http.StatusCreated {
			t.Errorf("%s with a valid reference: %d %v", name, code, out)
		}
	}
}
