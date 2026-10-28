package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Projects live on the global tier, inside an organization, in the global
// tier's objects. `state.mu` guards them, because listProjects reads them from
// the handler that also serves the compute tier.
//
// What this follows from the platform, because the provider branches on each:
//
//   - The map is keyed by id alone, so the fake cannot hold one id in two
//     organizations. The contract promises uniqueness only inside an
//     organization, and whether the platform enforces more is unconfirmed.
//   - A project in another organization answers exactly as one that does not
//     exist: the same 404 and the same body.
//   - `displayName` and `description` are always in the response, as "" when
//     unset.
//   - An id left out is generated as `proj-` and eight hex digits.
//   - A delete moves the project to `deleting` and it stays readable there
//     until the teardown finishes. A second delete, or a delete while the
//     project holds clusters, is a 409.
//
// There is no `creating` state: `ProjectStatus` declares none and the public
// create inserts the project `active`.
//
// Since v1.3.0-dev.4 a project is created in one region, which has
// to host projects, and locked to it. A create without one is the 422 the kind
// stack answered, `validation: region is required`. Every read carries the
// region, and a project made before regions were required carries none:
// legacyProject is one.
type fakeProject struct {
	id          string
	org         string
	region      string
	displayName string
	description string
	status      string
	createdAt   time.Time
	// Reads since the last DELETE while `deleting`. The teardown finishes on
	// the teardownReads-th, which leaves room for a destroy to find the
	// project still `deleting` after an earlier one gave up.
	readsSinceDelete int
}

const teardownReads = 3

// projectSeeds are the projects every new fake starts with. Each fake gets its
// own copy, so what one run does to them never reaches another.
var projectSeeds = []fakeProject{
	// Seeded, because `examples/demo` and the acceptance suite name this
	// project and neither creates it.
	{
		id: "demo", org: "org-demo", region: projectRegion, displayName: "Demo", status: "active",
		createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	},
	// A second project, seeded for the same reason. A create needs its
	// project to exist, as it does on a real landscape.
	{
		id: "example", org: "org-demo", region: projectRegion, displayName: "Example", status: "active",
		createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	},
	// A project made before regions were required, which reads back with no
	// region and isn't checked against the URL's.
	{
		id: legacyProject, org: "org-demo", displayName: "Before regions", status: "active",
		createdAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	},
}

// projectRegion is the region the fake's own projects are in, and the one
// regional region it serves.
const projectRegion = "us-texas-1"

// legacyProject is the seeded project with no region.
const legacyProject = "before-regions"

// regionIDPattern is the contract's pattern for ProjectCreateRequest.region.
var regionIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// fakeRegions is what listRegions answers. `global` is the control-plane tier
// alone here, as it is everywhere but the all-in-one install.
var fakeRegions = []map[string]any{
	{"id": "global", "type": "global", "hostsProjects": false},
	{"id": projectRegion, "type": "regional", "hostsProjects": true},
}

func hostsProjects(region string) bool {
	for _, r := range fakeRegions {
		if r["id"] == region {
			return r["hostsProjects"] == true
		}
	}
	return false
}

// The id pattern from the contract.
var projectIDPattern = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// The organizations a project can be created in. Creating one is operator-plane
// only, so from the provider's side an organization exists already or does not.
//
// `org-empty` holds no project, which is the case the projects data source
// cannot report and the organizations data source exists for.
func seededOrganizations() map[string]bool {
	return map[string]bool{"org-demo": true, "org-empty": true}
}

// ASSUMPTION: the list includes every organization to whoever holds the
// fake's token. On the k0rdent API v1.3.0-dev.3 a signed-in user's token is
// listed its organization and an API key is listed none, and a service
// account is unmeasured. The fake mints the same token for both grants, so it
// models the credential the list includes.
func organizationBody(id string) map[string]any {
	return map[string]any{
		"id":          id,
		"uid":         uidFor(scope{"global", ""}, "organization", id),
		"displayName": strings.TrimPrefix(id, "org-"),
		"status":      "active",
		"labels":      map[string]any{},
		"createdAt":   "2026-01-01T00:00:00Z",
		"selfLink":    "/v1/regions/global/organizations/" + id,
	}
}

// listOrganizations serves offset paging one row a page, as listProjects does
// and for the same reason, with the `status` filter the operation documents.
func (s *state) listOrganizations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Before the filter, as before: a request that mixes the two paging
	// styles is refused whatever it filters on.
	if organizationList.refusesMixed(w, q) {
		return
	}
	items := []any{}
	switch status := q.Get("status"); status {
	case "", "active":
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, id := range sortedMapKeys(s.organizations) {
			if scopedTo(r).seesOrganization(id) {
				items = append(items, organizationBody(id))
			}
		}
	case "suspended", "disabled", "provisioning", "failed", "deleting":
		// Every organization the fake knows is active, and an empty page is
		// the whole answer: no offset applies to it.
		writeJSON(w, http.StatusOK, map[string]any{"organizations": items, "totalCount": 0})
		return
	default:
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "status "+status+" is not one the list filters on")
		return
	}
	organizationList.serve(w, r, items)
}

func (s *state) getOrganization(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("orgId")
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.organizations[id] {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "not found: organization \""+id+"\"")
		return
	}
	writeJSON(w, http.StatusOK, organizationBody(id))
}

func (p *fakeProject) body() map[string]any {
	out := map[string]any{
		"id":             p.id,
		"organizationId": p.org,
		"status":         p.status,
		"displayName":    p.displayName,
		"description":    p.description,
		"createdAt":      p.createdAt.Format(time.RFC3339),
		"selfLink":       "/v1/regions/global/organizations/" + p.org + "/projects/" + p.id,
	}
	if p.region != "" {
		out["region"] = p.region
	}
	return out
}

func generatedProjectID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "proj-" + hex.EncodeToString(b)
}

// projectInOrg finds a project, answering the same 404 whether it is missing
// or in another organization. The caller holds s.mu.
func (s *state) projectInOrg(w http.ResponseWriter, r *http.Request) (*fakeProject, at, bool) {
	org, where := r.PathValue("orgId"), at{id: r.PathValue("project")}
	p, found := projectKind.rows(s.global())[where]
	if !found || p.org != org {
		projectKind.notFound(w, where.id)
		return nil, at{}, false
	}
	return p, where, true
}

func (s *state) createProjectInOrg(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("orgId")
	var body struct {
		ID          *string `json:"id"`
		DisplayName *string `json:"displayName"`
		Description *string `json:"description"`
		Region      *string `json:"region"`
	}
	if !decode(w, r, &body) {
		return
	}
	switch {
	case body.Region == nil || *body.Region == "":
		// Recorded on the kind stack: no reason.
		errorEnvelope(w, http.StatusUnprocessableEntity, "", "validation: region is required", nil)
		return
	case !regionIDPattern.MatchString(*body.Region):
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"validation: region "+*body.Region+" does not match the required pattern")
		return
	case !hostsProjects(*body.Region):
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"validation: region \""+*body.Region+"\" does not host projects")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.organizations[org] {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "not found: organization \""+org+"\"")
		return
	}
	id := generatedProjectID()
	if body.ID != nil && *body.ID != "" {
		id = *body.ID
		if !projectIDPattern.MatchString(id) || len(id) > 63 {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				"id "+id+" does not match the required pattern")
			return
		}
	}
	if _, found := projectKind.rows(s.global())[at{id: id}]; found {
		apiError(w, http.StatusConflict, "ALREADY_EXISTS", "conflict: slug \""+id+"\" already exists")
		return
	}

	p := &fakeProject{id: id, org: org, region: *body.Region, status: "active", createdAt: time.Now().UTC()}
	if body.DisplayName != nil {
		p.displayName = *body.DisplayName
	}
	if body.Description != nil {
		p.description = *body.Description
	}
	projectKind.put(s.global(), "", id, p)
	writeJSON(w, http.StatusCreated, p.body())
}

func (s *state) getProjectInOrg(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, where, ok := s.projectInOrg(w, r)
	if ok && projectKind.survives(w, s.global(), where, p) {
		writeJSON(w, http.StatusOK, p.body())
	}
}

func (s *state) updateProjectInOrg(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName *string `json:"displayName"`
		Description *string `json:"description"`
		Region      *string `json:"region"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Region != nil {
		errorEnvelope(w, http.StatusConflict, "CONFLICT_IMMUTABLE",
			"conflict: a project's region cannot be changed after it is created",
			[]any{map[string]any{"reason": "CONFLICT_IMMUTABLE"}})
		return
	}
	if body.DisplayName == nil && body.Description == nil {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "at least one field must be provided")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	p, _, ok := s.projectInOrg(w, r)
	if !ok {
		return
	}
	// A PATCH sets what it names, including to "", and leaves the rest.
	if body.DisplayName != nil {
		p.displayName = *body.DisplayName
	}
	if body.Description != nil {
		p.description = *body.Description
	}
	writeJSON(w, http.StatusOK, p.body())
}

func (s *state) deleteProjectInOrg(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, where, ok := s.projectInOrg(w, r)
	if !ok {
		return
	}
	p.readsSinceDelete = 0
	if p.status == "deleting" {
		// Recorded on v1.3.0-dev.4: no reason.
		recordedConflict(w, "conflict: project \""+p.id+"\" is already being deleted", "")
		return
	}
	// Clusters only, and in every region: the project is global and its
	// clusters are not. Nothing else holds a delete up, and nothing else is
	// cleared by one. The contract says a project's networks, subnets, VPCs
	// "and the like" are not removed and stay addressable under a project
	// re-created with the same id, so the fake leaves the scope where it is.
	held := 0
	for sc, o := range s.scopes {
		if sc.project != p.id {
			continue
		}
		held += len(clusterKind.under(o, ""))
	}
	if held > 0 {
		apiError(w, http.StatusConflict, "CONFLICT", "conflict: project \""+p.id+"\" still has "+
			strconv.Itoa(held)+" cluster(s); delete them before deleting the project")
		return
	}
	projectKind.tearDown(s.global(), where, p)
	w.WriteHeader(http.StatusNoContent)
}

// listProjectsInOrg is one organization's projects, with the `status` and
// `search` filters the operation documents. An organization that doesn't
// exist never reaches it: the gateway answers 403 first.
func (s *state) listProjectsInOrg(w http.ResponseWriter, r *http.Request) {
	org, q := r.PathValue("orgId"), r.URL.Query()
	status := q.Get("status")
	switch status {
	case "", "active", "suspended", "disabled", "deleting":
	default:
		// `deleted`, `provisioning` and `failed` are refused by name, and so
		// is anything else.
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"status "+status+" is not one a project can be listed by")
		return
	}
	search := strings.ToLower(q.Get("search"))

	s.mu.Lock()
	defer s.mu.Unlock()
	items := []any{}
	for _, p := range projectKind.under(s.global(), "") {
		if p.org != org || (status != "" && p.status != status) {
			continue
		}
		if search != "" && !strings.Contains(p.id, search) && !strings.Contains(strings.ToLower(p.displayName), search) {
			continue
		}
		items = append(items, p.body())
	}
	projectInOrgList.serve(w, r, items)
}
