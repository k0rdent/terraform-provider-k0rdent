package main

import (
	"net/http"
	"strings"
	"testing"
)

// The provider's lost-answer recovery rests on two things the platform does,
// so the fake has to do them too: a second create with the same id answers
// 409 and makes nothing, and a read gives back the tags a create sent.

// tagged is a scopedKinds create body with a tag added, or "" for a kind whose
// create takes none.
func tagged(kind, body string) string {
	switch kind {
	case "instance group", "file store":
		return ""
	}
	return strings.TrimSuffix(body, "}") + `,"tags":{"terraform-provider-k0rdent/create-token":"t1"}}`
}

func TestASecondCreateWithTheSameIDAnswers409AndMakesNothing(t *testing.T) {
	for _, k := range scopedKinds {
		t.Run(k.name, func(t *testing.T) {
			s := newState()
			h := k.routes(s)
			if k.parent != nil {
				k.parent(t, s)
			}
			if code, out := in(t, h.create, http.MethodPost, "r", "p", k.path, k.body); code != http.StatusCreated {
				t.Fatalf("first create: %d %v", code, out)
			}
			code, out := in(t, h.create, http.MethodPost, "r", "p", k.path, k.body)
			if code != http.StatusConflict || !strings.Contains(errorMessage(out), "already exists") {
				t.Errorf("second create: %d %v, want 409 already exists", code, out)
			}
			if _, list := in(t, h.list, http.MethodGet, "r", "p", k.path, ""); listed(list, k.listKey) != 1 {
				t.Errorf("list after two creates: %v, want one", list)
			}
		})
	}
}

func TestACreatesTagsReadBack(t *testing.T) {
	for _, k := range scopedKinds {
		body := tagged(k.name, k.body)
		if body == "" {
			continue
		}
		t.Run(k.name, func(t *testing.T) {
			s := newState()
			h := k.routes(s)
			if k.parent != nil {
				k.parent(t, s)
			}
			if code, out := in(t, h.create, http.MethodPost, "r", "p", k.path, body); code != http.StatusCreated {
				t.Fatalf("create: %d %v", code, out)
			}
			_, out := in(t, h.get, http.MethodGet, "r", "p", withID(k.path, "x"), "")
			tags, ok := out["tags"].(map[string]any)
			if !ok || tags["terraform-provider-k0rdent/create-token"] != "t1" {
				t.Errorf("tags read back = %v, want the one sent", out["tags"])
			}
		})
	}
}
