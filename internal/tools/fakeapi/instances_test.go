package main

import (
	"bytes"
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

// power sends one power action to an instance in the scope groupHolding uses.
func power(s *state, id, verb string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.SetPathValue("id", id)
	r.SetPathValue("action", verb)
	rec := httptest.NewRecorder()
	s.executeInstancePowerAction(rec, r)
	return rec
}

// A power action is accepted once and answers the instance as it was, a
// second inside the guard interval is refused with the wait, and a host that
// can't be powered, or isn't there, is refused for that.
func TestAPowerActionIsAcceptedOnceAndGuarded(t *testing.T) {
	s := newState()
	g := groupHolding(s, "trainers", map[string]int32{"gpu": 1})
	o := s.in(httptest.NewRequest(http.MethodGet, "/", nil))
	id := o.instancesOf(g)[0].id

	if rec := power(s, id, "explode"); rec.Code != http.StatusBadRequest {
		t.Errorf("an action outside the vocabulary answered %d, want 400", rec.Code)
	}
	rec := power(s, id, "powerCycle")
	var accepted struct {
		ID    string  `json:"id"`
		State string  `json:"state"`
		Error *string `json:"lastPowerActionError"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || rec.Code != http.StatusAccepted ||
		accepted.ID != id || accepted.State != "running" || accepted.Error != nil {
		t.Fatalf("first action: %d %s", rec.Code, rec.Body)
	}

	rec = power(s, id, "powerOff")
	var refused struct {
		Error struct {
			Details []struct {
				Reason   string         `json:"reason"`
				Metadata map[string]any `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil || rec.Code != http.StatusConflict ||
		len(refused.Error.Details) != 1 || refused.Error.Details[0].Reason != "POWER_ACTION_IN_FLIGHT" ||
		refused.Error.Details[0].Metadata["retryAfterSeconds"] == nil || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second action: %d %s, Retry-After %q", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}

	if rec := power(s, "inst-00000000000000000000000000000000", "powerOn"); rec.Code != http.StatusNotFound {
		t.Errorf("an instance that isn't there answered %d, want 404", rec.Code)
	}
	name := "x " + powerUnsupported
	g.displayName = &name
	if rec := power(s, id, "powerOn"); rec.Code != http.StatusConflict ||
		!bytes.Contains(rec.Body.Bytes(), []byte("POWER_ACTION_UNSUPPORTED")) {
		t.Errorf("a host with no power control: %d %s", rec.Code, rec.Body)
	}
}

// A host whose group carries powerFails reports lastPowerActionError once an
// action has been accepted for it, and not before, not even in the 202.
func TestAFailedPowerActionShowsOnTheInstance(t *testing.T) {
	s := newState()
	g := groupHolding(s, "trainers", map[string]int32{"gpu": 1})
	name := "x " + powerFails
	g.displayName = &name
	o := s.in(httptest.NewRequest(http.MethodGet, "/", nil))
	id := o.instancesOf(g)[0].id
	if got := o.instancesOf(g)[0].powerError; got != "" {
		t.Fatalf("a failure before any action: %q", got)
	}
	if rec := power(s, id, "powerOff"); rec.Code != http.StatusAccepted ||
		bytes.Contains(rec.Body.Bytes(), []byte("lastPowerActionError")) {
		t.Fatalf("the 202 is the instance at submission, with no failure yet: %d %s", rec.Code, rec.Body)
	}
	if got := o.instancesOf(g)[0].body("r", "p")["lastPowerActionError"]; got != powerFailure {
		t.Errorf("after the action the instance reports %v, want %q", got, powerFailure)
	}
}

// A deleted instance's record answers 410, not 404, to a power action and to
// a read, as the service does, and a read with showDeleted returns it.
func TestADeletedInstanceIsGone(t *testing.T) {
	s := newState()
	o := s.in(httptest.NewRequest(http.MethodGet, "/", nil))
	old := groupHolding(s, "trainers", map[string]int32{"gpu": 1})
	instanceGroupKind.drop(o, at{id: old.id}, old)
	id := o.released[0].id

	if rec := power(s, id, "powerOn"); rec.Code != http.StatusGone ||
		!bytes.Contains(rec.Body.Bytes(), []byte(`"GONE"`)) {
		t.Errorf("a power action on a deleted instance: %d %s", rec.Code, rec.Body)
	}
	for query, want := range map[string]int{"": http.StatusGone, "?showDeleted=true": http.StatusOK} {
		r := httptest.NewRequest(http.MethodGet, "/"+query, nil)
		r.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		s.getInstance(rec, r)
		if rec.Code != want {
			t.Errorf("reading a deleted instance with %q answered %d, want %d: %s", query, rec.Code, want, rec.Body)
		}
	}
}
