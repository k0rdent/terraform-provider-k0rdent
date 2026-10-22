package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// An id that isn't one URL segment never reaches the platform, wherever it
// came from: state, an import id, a reference or a platform answer. Plan-time
// validatePathSegment catches a value a configuration sets. Under it, the
// generated client refuses the rest as it builds the URL, and this holds the
// client newMeta builds to that, so a wrapper added around it later can't
// quietly skip the check.
func TestTheClientRefusesAPathParameterThatIsNotOneSegment(t *testing.T) {
	var sent atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
	}))
	t.Cleanup(server.Close)
	meta, err := newMeta(Settings{Endpoint: server.URL, Credential: CredentialSource{Kind: APIKeyCredential, APIKey: "k"}}, "test")
	if err != nil {
		t.Fatalf("new meta: %v", err)
	}

	for _, id := range []string{"..", ".", ""} {
		_, err := meta.Client.DeleteClusterWithResponse(context.Background(), "us-west", "team-a", id)
		var refused *k0rdent.PathParamError
		if !errors.As(err, &refused) || refused.Operation != "deleteCluster" || refused.Param != "id" {
			t.Errorf("delete cluster %q: err = %v, want the client's refusal of id", id, err)
		}
	}
	if n := sent.Load(); n > 0 {
		t.Errorf("sent %d requests", n)
	}
}
