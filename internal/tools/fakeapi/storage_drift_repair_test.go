package main

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

func fileStoreConsumerStatus(t *testing.T, store map[string]any) map[string]any {
	t.Helper()
	statuses, ok := store["consumerStatus"].([]any)
	if !ok || len(statuses) != 1 {
		t.Fatalf("consumerStatus = %v, want one entry", store["consumerStatus"])
	}
	status, ok := statuses[0].(map[string]any)
	if !ok {
		t.Fatalf("consumerStatus[0] = %T, want an object", statuses[0])
	}
	return status
}

func fileStoreResolvedAddresses(t *testing.T, store map[string]any) []string {
	t.Helper()
	values, ok := fileStoreConsumerStatus(t, store)["resolvedAddresses"].([]any)
	if !ok {
		t.Fatalf("resolvedAddresses = %v, want a list", fileStoreConsumerStatus(t, store)["resolvedAddresses"])
	}
	addresses := make([]string, 0, len(values))
	for _, value := range values {
		address, ok := value.(string)
		if !ok {
			t.Fatalf("address = %T, want a string", value)
		}
		addresses = append(addresses, address)
	}
	return addresses
}

func TestMidScaleResolveBeforeV131KeepsBaseAddresses(t *testing.T) {
	s := newState()
	body := `{"id":"g","clusterType":"/v1/regions/r/compute/cluster-types/cpu-general","nodePools":[{"nodePool":"workers","size":1}]}`
	if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	g := map[string]string{"id": "g"}
	for range settleAfter + 1 {
		in(t, s.getInstanceGroup, http.MethodGet, "r", "p", g, "")
	}
	if code, out := in(t, s.scaleInstanceGroup, http.MethodPost, "r", "p", g, `{"nodePools":[{"nodePool":"workers","size":2}]}`); code != http.StatusOK {
		t.Fatalf("scale: %d %v", code, out)
	}

	o := s.scopes[scope{region: "r", project: "p"}]
	kind := "instanceGroup"
	_, statuses, refusal := o.resolveConsumers([]fakeConsumer{{
		Ref: "g", Type: &kind, Network: &fakeConsumerNetwork{Type: "ethernet"},
	}})
	want := []string{"10.80.11.1", "10.80.11.2"}
	if refusal != "" {
		t.Fatalf("refusal = %q, want addresses %v", refusal, want)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %v, want one", statuses)
	}
	if !reflect.DeepEqual(statuses[0].addresses, want) {
		t.Fatalf("addresses = %v, want %v", statuses[0].addresses, want)
	}
}

func TestV131InfiniBandRepairKeepsFabric(t *testing.T) {
	s := newState()
	body := `{"id":"g","clusterType":"/v1/regions/r/compute/cluster-types/cpu-general",` +
		`"nodePools":[{"nodePool":"workers","size":1}],"displayName":"` + apiV131 + `"}`
	if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	g := map[string]string{"id": "g"}
	for range settleAfter + 1 {
		in(t, s.getInstanceGroup, http.MethodGet, "r", "p", g, "")
	}
	if code, out := in(t, s.createFileStore, http.MethodPost, "r", "p", nil,
		`{"id":"fs","shareType":"general","capacityGiB":10,"consumers":[{"ref":"g","type":"instanceGroup","network":{"type":"infiniband"}}]}`); code != http.StatusCreated {
		t.Fatalf("create store: %d %v", code, out)
	}
	fs := map[string]string{"id": "fs"}
	for range settleAfter + 3 {
		in(t, s.getFileStore, http.MethodGet, "r", "p", fs, "")
	}
	if code, out := in(t, s.scaleInstanceGroup, http.MethodPost, "r", "p", g, `{"nodePools":[{"nodePool":"workers","size":2}]}`); code != http.StatusOK {
		t.Fatalf("scale: %d %v", code, out)
	}
	for range settleAfter + 1 {
		in(t, s.getInstanceGroup, http.MethodGet, "r", "p", g, "")
	}

	_, before := in(t, s.getFileStore, http.MethodGet, "r", "p", fs, "")
	if got := fileStoreConsumerStatus(t, before)["state"]; got != "active" {
		t.Fatalf("state before the drift interval = %v, want active with the old grant", got)
	}
	if got, want := fileStoreResolvedAddresses(t, before), []string{"10.81.11.1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses before the drift interval = %v, want %v", got, want)
	}

	time.Sleep(2100 * time.Millisecond)
	var repaired map[string]any
	for range settleAfter + 4 {
		_, repaired = in(t, s.getFileStore, http.MethodGet, "r", "p", fs, "")
	}
	if got := fileStoreConsumerStatus(t, repaired)["state"]; got != "active" {
		t.Fatalf("repaired state = %v, want active", got)
	}
	if got, want := fileStoreResolvedAddresses(t, repaired), []string{"10.81.11.1", "10.81.11.2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("repaired addresses = %v, want InfiniBand addresses %v", got, want)
	}
}
