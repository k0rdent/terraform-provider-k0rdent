package provider

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// clientOwnOperations are the operations the client sends for itself: the
// token mint behind every authenticated call.
var clientOwnOperations = []string{"mintToken"}

// The client in internal/k0rdent is generated from the operations in its
// spec/operations.txt, and the source this repository publishes should hold
// no operation the provider doesn't send. A call to one that's missing fails
// to compile. This fails the other way: an operation the provider stopped
// calling, still generated.
func TestTheClientHoldsOnlyTheOperationsTheProviderCalls(t *testing.T) {
	want := append(providerOperations(t), clientOwnOperations...)
	var extra []string
	for id := range k0rdent.Operations {
		if !slices.Contains(want, id) {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("the client has operations nothing here calls. Take them out of "+
			"internal/k0rdent/spec/operations.txt and run `make client-sync`:\n  %s",
			strings.Join(extra, "\n  "))
	}
	for _, id := range want {
		if _, ok := k0rdent.Operations[id]; !ok {
			t.Errorf("the provider calls %s, which the client's table doesn't have", id)
		}
	}
}
