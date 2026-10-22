package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

// groupHolding puts a provisioned group in the scope listAll addresses.
func groupHolding(s *state, id string, held map[string]int32) *instanceGroup {
	o := s.in(httptest.NewRequest(http.MethodGet, "/", nil))
	g := &instanceGroup{id: id, uid: uidFor(o.scope, "instance_group", id), state: "active",
		pools: held, held: held}
	o.groups[at{id: id}] = g
	return g
}

// Instances are derived from what each group holds, page one at a time on the
// cursor, and refuse a page larger than the contract's 200.
func TestInstancesComeFromWhatEachGroupHolds(t *testing.T) {
	s := newState()
	groupHolding(s, "trainers", map[string]int32{"gpu": 2, "cpu": 1})
	groupHolding(s, "loaders", map[string]int32{"cpu": 1})

	rec := httptest.NewRecorder()
	s.listInstances(rec, httptest.NewRequest(http.MethodGet, "/?pageSize=500", nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("pageSize 500 answered %d, want the contract's 422", rec.Code)
	}

	var ids []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the list never ended")
		}
		rec := httptest.NewRecorder()
		s.listInstances(rec, httptest.NewRequest(http.MethodGet, "/?pageSize=200&instanceGroupId=trainers&pageToken="+token, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		var out struct {
			Instances []struct {
				ID                   string `json:"id"`
				InstanceGroupID      string `json:"instanceGroupId"`
				NetworkConfiguration []struct {
					Type, MacAddress, GUID string
				} `json:"networkConfiguration"`
			} `json:"instances"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, in := range out.Instances {
			if in.InstanceGroupID != "trainers" || len(in.NetworkConfiguration) != 2 {
				t.Errorf("instance %s: group %q, %d interfaces", in.ID, in.InstanceGroupID, len(in.NetworkConfiguration))
			}
			ids = append(ids, in.ID)
		}
		if out.NextPageToken == "" {
			break
		}
		token = out.NextPageToken
	}
	if len(ids) != 3 {
		t.Errorf("trainers holds %v, want its three hosts", ids)
	}
}

// A grant narrowed to one MAC address grants that host's address and nothing
// else, and one naming no interface in the group is refused, as the contract
// says.
func TestANarrowedGrantMatchesOneInterface(t *testing.T) {
	s := newState()
	g := groupHolding(s, "trainers", map[string]int32{"gpu": 2})
	o := s.in(httptest.NewRequest(http.MethodGet, "/", nil))
	hosts := o.instancesOf(g)

	mac := hosts[1].mac
	got, ok := o.narrowedAddresses(g, fakeConsumerNetwork{Type: "ethernet", MacAddress: &mac})
	if !ok || len(got) != 1 || got[0] != hosts[1].ethernet {
		t.Errorf("narrowed to %s: got %v, want [%s]", mac, got, hosts[1].ethernet)
	}

	guid := hosts[0].guid
	got, ok = o.narrowedAddresses(g, fakeConsumerNetwork{Type: "infiniband", GUID: &guid})
	if !ok || len(got) != 1 || got[0] != hosts[0].ibA {
		t.Errorf("narrowed to %s: got %v, want [%s]", guid, got, hosts[0].ibA)
	}

	other := "02:00:00:00:00:00"
	if _, ok := o.narrowedAddresses(g, fakeConsumerNetwork{Type: "ethernet", MacAddress: &other}); ok {
		t.Error("a MAC address on no member matched")
	}
}

// instanceStates lists every page of the scope's instances for one query and
// returns their states, sorted.
func instanceStates(t *testing.T, s *state, query string) []string {
	t.Helper()
	var got []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("the list never ended")
		}
		rec := httptest.NewRecorder()
		s.listInstances(rec, httptest.NewRequest(http.MethodGet, "/?pageSize=200&pageToken="+token+query, nil))
		var out struct {
			Instances     []struct{ State string } `json:"instances"`
			NextPageToken string                   `json:"nextPageToken"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", rec.Body, err)
		}
		for _, in := range out.Instances {
			got = append(got, in.State)
		}
		if out.NextPageToken == "" {
			break
		}
		token = out.NextPageToken
	}
	sort.Strings(got)
	return got
}

// `inStates` keeps the hosts in any state it names. Every host here is
// running, so naming only another state answers none, and naming running
// among others answers all of them.
func TestInstancesNarrowToTheStatesAskedFor(t *testing.T) {
	s := newState()
	groupHolding(s, "trainers", map[string]int32{"gpu": 1, "cpu": 1})

	if got := fmt.Sprint(instanceStates(t, s, "")); got != "[running running]" {
		t.Fatalf("the group's hosts read as %s, want two running", got)
	}
	if got := instanceStates(t, s, "&inStates=stopped"); len(got) != 0 {
		t.Errorf("inStates=stopped answered %v, want none", got)
	}
	if got := fmt.Sprint(instanceStates(t, s, "&inStates=stopped&inStates=running")); got != "[running running]" {
		t.Errorf("stopped or running answered %s, want both hosts", got)
	}
}

// An instance whose group was deleted is kept as a deleted record, which
// only `showDeleted=true` or `inStates=deleted` returns. The contract turns
// `showDeleted` on for `inStates=deleted`, so that alone finds it. A group
// of the same id made again has live instances beside the records.
func TestADeletedGroupLeavesDeletedRecords(t *testing.T) {
	s := newState()
	o := s.in(httptest.NewRequest(http.MethodGet, "/", nil))
	old := groupHolding(s, "trainers", map[string]int32{"gpu": 1})
	instanceGroupKind.drop(o, at{id: old.id}, old)
	groupHolding(s, "trainers", map[string]int32{"gpu": 1})

	for query, want := range map[string]string{
		"":                                   "[running]",
		"&showDeleted=true":                  "[deleted running]",
		"&inStates=deleted":                  "[deleted]",
		"&inStates=running":                  "[running]",
		"&inStates=deleted&inStates=running": "[deleted running]",
	} {
		if got := fmt.Sprint(instanceStates(t, s, query+"&instanceGroupId=trainers")); got != want {
			t.Errorf("%q answered %s, want %s", query, got, want)
		}
	}
}
