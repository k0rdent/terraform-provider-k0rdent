package main

import "net/http"

// storeRead answers a GET of an API path from the fake's own store, as the
// service behind the gateway would, with no bearer check and no gateway
// authorization in front of it.
//
// It's for the harness alone, behind the owner token. A test that has
// destroyed a project still has to show that each object that was in it is
// gone, and the API can't tell it: the gateway answers 403 for anything under
// a project that doesn't exist, as v1.3.0-dev.4 does, whether the object was
// deleted or left behind. The store can. An object left behind still reads
// 200 here, so the check is as strict as it was.
//
// A read here moves a row a step, as an API read does.
func (s *state) storeRead() http.HandlerFunc {
	store := http.NewServeMux()
	s.routesWith(store, func(next http.HandlerFunc) http.HandlerFunc { return next })
	// The API has no read of one webhook subscription, and a test still has
	// to show one is gone, or that a rotation changed the secret the platform
	// signs with. So the store answers it here, and only here.
	store.HandleFunc("GET /v1/regions/global/notifications/webhooks/{id}", s.storedWebhook)
	return func(w http.ResponseWriter, r *http.Request) {
		inner := r.Clone(r.Context())
		inner.URL.Path = "/" + r.PathValue("path")
		inner.URL.RawPath = ""
		store.ServeHTTP(w, inner)
	}
}
