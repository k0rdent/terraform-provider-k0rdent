package provider

import (
	"context"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// An entry with no ref has no key to sit under. Keyed by "" it would collide
// with any other such entry and the map would keep whichever came last.
func TestFileStoreDataSkipsAConsumerStatusWithNoRef(t *testing.T) {
	f := decodeInto[k0rdent.FileStore](t, `{"id":"fs","consumerStatus":[
		{"state":"failed"},
		{"ref":"","state":"creating"},
		{"ref":"10.0.0.31","state":"active","resolvedAddresses":["10.0.0.31"]}]}`)

	m, diags := fileStoreDataConsumers(context.Background(), f)
	if diags.HasError() {
		t.Fatal(diags)
	}
	elems := m.Elements()
	if _, ok := elems[""]; ok || len(elems) != 1 {
		t.Fatalf("consumer_status keys = %v, want only 10.0.0.31", keysOf(elems))
	}
	if _, ok := elems["10.0.0.31"]; !ok {
		t.Fatalf("consumer_status lost the entry that has a ref: %v", keysOf(elems))
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
