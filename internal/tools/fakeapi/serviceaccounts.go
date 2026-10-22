package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"regexp"
	"slices"
	"time"
)

// Service accounts and their credentials live on the global tier, inside an
// organization, as projects do. What the fake follows from the contract,
// because the provider branches on each:
//
//   - A create takes `id`, which has to match `sa-[a-z0-9]+(-[a-z0-9]+)*`, or
//     is generated as `sa-` and eight hex digits. A taken id is a 409.
//   - `clientId` is the server's own and never changes. It's the id at the
//     scope id, as dev.4 answered it.
//   - The create takes no `status`: an account is made `active`, and only a
//     PATCH moves it.
//   - A delete is permanent and immediate, a 204 with nothing left to read,
//     and takes the account's credentials with it.
//   - A credential's id is the server's, `cred-` and 32 hex digits. Its secret
//     is in the create's answer and nowhere else, ever: the fake keeps only a
//     hash of it, as a server should.
//   - At most five credentials whose `expiresAt` hasn't passed, and a sixth is
//     a 409. One left out of `expiresAt` lasts 90 days, and one in the past is
//     a 422.
//
// The tables sit in the objects of the scope the global routes name, which is
// no region and no project, so the object table's lookups work on them as on
// any other kind. They're keyed by id alone, which takes the same view as
// projects.go does: the contract promises uniqueness inside an organization,
// and an account in another organization answers exactly as one that doesn't
// exist.
//
// A credential authenticates: the token endpoint takes a service account's
// client id and a credential's secret, so a test can show the secret it was
// handed is the one the platform issued. See serviceAccountMints.

var (
	serviceAccountKind = &kind[*fakeServiceAccount]{
		name: "service account",
		rows: func(o *objects) table[*fakeServiceAccount] { return o.serviceAccounts },
		gone: func(o *objects, sa *fakeServiceAccount) {
			for where := range o.credentials {
				if where.parent == sa.id {
					delete(o.credentials, where)
				}
			}
		},
		duplicate: func(w http.ResponseWriter, id string) {
			apiError(w, http.StatusConflict, "ALREADY_EXISTS", "conflict: service account \""+id+"\" already exists")
		},
	}

	// A credential missing from an account that's there names itself. One
	// under an account that isn't names the account, which is the parent rule
	// every nested kind here follows.
	credentialKind = &kind[*fakeCredential]{
		name:        "credential",
		parent:      serviceAccountKind,
		parentParam: "serviceAccountId",
		rows:        func(o *objects) table[*fakeCredential] { return o.credentials },
	}
)

// The patterns and limits from the contract.
var (
	serviceAccountIDPattern = regexp.MustCompile(`^sa-[a-z0-9]+(-[a-z0-9]+)*$`)
	serviceAccountScopes    = []string{"organization", "folder", "project"}
)

const (
	// issuedSecretPrefix starts every secret a credential is issued.
	issuedSecretPrefix   = "k0r_fake_sa_secret_"
	credentialLifetime   = 90 * 24 * time.Hour
	activeCredentialsMax = 5
)

// fakeRoles are the roles every organization defines, as listRoles answers
// them: an id, a display name and the tiers each binds at. A service account
// names a role by its id, not the dotted name in the contract's example.
//
// ASSUMPTION: every role here can be granted. The platform grants only roles
// the caller holds at the scope, and the fake's caller holds everything. No
// tier check either: whether binding a role at a tier its `scopes` leave out
// is refused wasn't measured.
type fakeRole struct {
	id, name string
	scopes   []string
}

var fakeRoles = []fakeRole{
	{"cluster-admin", "Cluster Admin", []string{"platform"}},
	{"compute-admin", "Compute Admin", []string{"platform", "org", "project"}},
	{"compute-member", "Compute Member", []string{"platform", "org", "project"}},
	{"compute-viewer", "Compute Viewer", []string{"platform", "org", "project"}},
	{"credentials-admin", "Credentials Admin", []string{"platform", "project"}},
	{"credentials-consumer", "Credentials Consumer", []string{"platform", "project"}},
	{"iam-admin", "IAM Admin", []string{"platform", "org"}},
	{"iam-viewer", "IAM Viewer", []string{"platform", "org"}},
	{"infrastructure-admin", "Infrastructure Admin", []string{"platform"}},
	{"infrastructure-member", "Infrastructure Member", []string{"platform"}},
	{"infrastructure-viewer", "Infrastructure Viewer", []string{"platform"}},
	{"organizations-admin", "Organizations Admin", []string{"platform", "org"}},
	{"organizations-member", "Organizations Member", []string{"platform", "org"}},
	{"organizations-viewer", "Organizations Viewer", []string{"platform", "org"}},
	{"platform-admin", "Platform Admin", []string{"platform"}},
	{"quota-admin", "Quota Admin", []string{"platform"}},
	{"storage-admin", "Storage Admin", []string{"platform"}},
}

func roleExists(id string) bool {
	return slices.ContainsFunc(fakeRoles, func(r fakeRole) bool { return r.id == id })
}

// listRoles answers every role, sorted by id, paged like the other
// organization lists.
func (s *state) listRoles(w http.ResponseWriter, r *http.Request) {
	if roleList.refusesMixed(w, r.URL.Query()) {
		return
	}
	org := r.PathValue("orgId")
	items := []any{}
	for _, role := range fakeRoles {
		items = append(items, map[string]any{
			"id":          role.id,
			"uid":         uidFor(scope{"global", org}, "role", role.id),
			"displayName": role.name,
			"permissions": []string{},
			"scopes":      role.scopes,
			"createdAt":   fixedTime,
			"version":     1,
			"selfLink":    "/v1/regions/global/organizations/" + org + "/iam/roles/" + role.id,
		})
	}
	roleList.serve(w, r, items)
}

type fakeServiceAccount struct {
	id, org     string
	displayName string
	description string
	scope       string
	scopeID     string
	roles       []string
	status      string
	createdAt   time.Time
	updatedAt   time.Time
}

type fakeCredential struct {
	id, account string
	// secretHash is all the fake keeps of the secret.
	secretHash [32]byte
	expiresAt  time.Time
	createdAt  time.Time
}

// An account and a credential have no transient state and no display name a
// marker could ride in. A credential's status is worked out from `expiresAt`
// on every read, so it has nothing stored for the table to move either.
func (sa *fakeServiceAccount) life() (*string, *int, *string) { return &sa.status, new(int), nil }
func (c *fakeCredential) life() (*string, *int, *string) {
	state := "active"
	return &state, new(int), nil
}

// expired is whether the credential has passed `expiresAt`. The contract says
// that, not the stored status, is what decides whether it authenticates.
func (c *fakeCredential) expired(now time.Time) bool { return !now.Before(c.expiresAt) }

// clientID is `<id>@<scopeId>.iam`: `@acme.iam` for an account scoped to
// organization `acme`, `@acme-dev.iam` for one scoped to project `acme-dev`.
func (sa *fakeServiceAccount) clientID() string { return sa.id + "@" + sa.scopeID + ".iam" }

func (sa *fakeServiceAccount) body(o *objects) map[string]any {
	roles := sa.roles
	if roles == nil {
		roles = []string{}
	}
	return map[string]any{
		"id":                    sa.id,
		"uid":                   uidFor(scope{"global", sa.org}, "service account", sa.id),
		"displayName":           sa.displayName,
		"description":           nullIfEmpty(sa.description),
		"clientId":              sa.clientID(),
		"scope":                 sa.scope,
		"scopeId":               sa.scopeID,
		"roles":                 roles,
		"status":                sa.status,
		"createdBy":             "usr-fake-admin",
		"createdAt":             sa.createdAt.Format(time.RFC3339),
		"updatedAt":             sa.updatedAt.Format(time.RFC3339),
		"activeCredentialCount": sa.activeCredentials(o, time.Now()),
		"selfLink":              "/v1/regions/global/organizations/" + sa.org + "/iam/service-accounts/" + sa.id,
	}
}

func (sa *fakeServiceAccount) activeCredentials(o *objects, now time.Time) int {
	n := 0
	for _, c := range credentialKind.under(o, sa.id) {
		if !c.expired(now) {
			n++
		}
	}
	return n
}

func (c *fakeCredential) body(org string, now time.Time) map[string]any {
	status := "active"
	if c.expired(now) {
		status = "expired"
	}
	return map[string]any{
		"id":               c.id,
		"uid":              uidFor(scope{"global", org}, "credential", c.id),
		"serviceAccountId": c.account,
		"status":           status,
		"expiresAt":        c.expiresAt.Format(time.RFC3339),
		"createdBy":        "usr-fake-admin",
		"createdAt":        c.createdAt.Format(time.RFC3339),
		"selfLink": "/v1/regions/global/organizations/" + org + "/iam/service-accounts/" + c.account +
			"/credentials/" + c.id,
	}
}

// accountInOrg finds the account a request's path names, answering the same 404
// whether it's missing or in another organization. param is the path value
// that names it. The caller holds s.mu.
func (s *state) accountInOrg(w http.ResponseWriter, r *http.Request, param string) (*objects, *fakeServiceAccount, at, bool) {
	o := s.in(r)
	where := at{id: r.PathValue(param)}
	sa, found := serviceAccountKind.rows(o)[where]
	if !found || sa.org != r.PathValue("orgId") {
		serviceAccountKind.notFound(w, where.id)
		return nil, nil, at{}, false
	}
	return o, sa, where, true
}

// accountFields are the fields a create and a PATCH both take, checked as the
// contract bounds them. It writes the 422 and reports false for the first one
// that's out of bounds.
func accountFields(w http.ResponseWriter, displayName, description *string, roles *[]string) bool {
	switch {
	case displayName != nil && (*displayName == "" || len(*displayName) > 255):
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "displayName must be 1 to 255 characters")
		return false
	case description != nil && len(*description) > 1024:
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "description must be at most 1024 characters")
		return false
	}
	if roles != nil {
		for _, role := range *roles {
			if !roleExists(role) {
				// Recorded on dev.4, with no reason.
				errorEnvelope(w, http.StatusUnprocessableEntity, "", "validation error: role \""+role+"\" does not exist", nil)
				return false
			}
		}
	}
	return true
}

// distinct is a role list as the platform keeps it: each slug once, in the
// order first given. ASSUMPTION: the contract types `roles` as an array and
// says nothing about repeats or order.
func distinct(roles []string) []string {
	out := []string{}
	for _, r := range roles {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

func (s *state) createServiceAccount(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("orgId")
	var body struct {
		ID          *string   `json:"id"`
		DisplayName *string   `json:"displayName"`
		Description *string   `json:"description"`
		Scope       *string   `json:"scope"`
		ScopeID     *string   `json:"scopeId"`
		Roles       *[]string `json:"roles"`
	}
	if !decode(w, r, &body) {
		return
	}
	switch {
	case body.DisplayName == nil || body.Scope == nil || body.ScopeID == nil:
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "displayName, scope and scopeId are required")
		return
	case !slices.Contains(serviceAccountScopes, *body.Scope):
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "scope "+*body.Scope+" is not one of organization, folder, project")
		return
	case *body.ScopeID == "":
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "scopeId is required")
		return
	case !accountFields(w, body.DisplayName, body.Description, body.Roles):
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// ASSUMPTION: a scope target the organization doesn't hold is the 422
	// the contract gives for the rest of a create's validation. The contract
	// says only that it "must be a resource the caller has access to". The
	// fake holds no folders, so a folder id isn't checked.
	switch *body.Scope {
	case "organization":
		if *body.ScopeID != org {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				"an organization-scoped service account's scopeId is its organization, "+org)
			return
		}
	case "project":
		if p, ok := projectKind.rows(s.global())[at{id: *body.ScopeID}]; !ok || p.org != org {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				"scopeId \""+*body.ScopeID+"\" is not a project in organization "+org)
			return
		}
	}

	id := ""
	if body.ID != nil && *body.ID != "" {
		id = *body.ID
		if !serviceAccountIDPattern.MatchString(id) || len(id) < 4 || len(id) > 63 {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				"id "+id+" does not match sa-[a-z0-9]+(-[a-z0-9]+)*")
			return
		}
	} else {
		id = "sa-" + randomHexString(4)
	}
	o := s.in(r)
	if _, ok := serviceAccountKind.claim(s, w, o, "", &id); !ok {
		return
	}

	now := time.Now().UTC().Truncate(time.Second)
	sa := &fakeServiceAccount{
		id: id, org: org, displayName: *body.DisplayName, scope: *body.Scope, scopeID: *body.ScopeID,
		status: "active", createdAt: now, updatedAt: now,
	}
	if body.Description != nil {
		sa.description = *body.Description
	}
	if body.Roles != nil {
		sa.roles = distinct(*body.Roles)
	}
	serviceAccountKind.put(o, "", id, sa)
	writeJSON(w, http.StatusCreated, sa.body(o))
}

func (s *state) getServiceAccount(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, sa, _, ok := s.accountInOrg(w, r, "id"); ok {
		writeJSON(w, http.StatusOK, sa.body(o))
	}
}

func (s *state) updateServiceAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName *string   `json:"displayName"`
		Description *string   `json:"description"`
		Roles       *[]string `json:"roles"`
		Status      *string   `json:"status"`
	}
	if !decode(w, r, &body) {
		return
	}
	switch {
	case body.DisplayName == nil && body.Description == nil && body.Roles == nil && body.Status == nil:
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "at least one field must be provided")
		return
	case body.Status != nil && *body.Status != "active" && *body.Status != "disabled":
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "status "+*body.Status+" is not one of active, disabled")
		return
	case !accountFields(w, body.DisplayName, body.Description, body.Roles):
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, sa, _, ok := s.accountInOrg(w, r, "id")
	if !ok {
		return
	}
	// A PATCH sets what it names, including to "" or [], and leaves the rest.
	if body.DisplayName != nil {
		sa.displayName = *body.DisplayName
	}
	if body.Description != nil {
		sa.description = *body.Description
	}
	if body.Roles != nil {
		sa.roles = distinct(*body.Roles)
	}
	if body.Status != nil {
		sa.status = *body.Status
	}
	sa.updatedAt = time.Now().UTC().Truncate(time.Second)
	writeJSON(w, http.StatusOK, sa.body(o))
}

func (s *state) deleteServiceAccount(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, sa, where, ok := s.accountInOrg(w, r, "id")
	if !ok {
		return
	}
	serviceAccountKind.tearDown(o, where, sa)
	w.WriteHeader(http.StatusNoContent)
}

// listServiceAccounts requires both `scope` and `scopeId`, as the contract
// does, so there is no listing every account in an organization at once.
func (s *state) listServiceAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if serviceAccountList.refusesMixed(w, q) {
		return
	}
	scopeName, scopeID, status := q.Get("scope"), q.Get("scopeId"), q.Get("status")
	switch {
	case !slices.Contains(serviceAccountScopes, scopeName) || scopeID == "":
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "scope and scopeId are required")
		return
	case status != "" && status != "active" && status != "disabled":
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "status "+status+" is not one of active, disabled")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, org := s.in(r), r.PathValue("orgId")
	items := []any{}
	for _, sa := range serviceAccountKind.under(o, "") {
		if sa.org != org || sa.scope != scopeName || sa.scopeID != scopeID || (status != "" && sa.status != status) {
			continue
		}
		items = append(items, sa.body(o))
	}
	serviceAccountList.serve(w, r, items)
}

func (s *state) createServiceAccountCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	// The body is optional. An empty one asks for the default lifetime.
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	if body.ExpiresAt != nil && !body.ExpiresAt.After(now) {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "expiresAt is in the past")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, sa, _, ok := s.accountInOrg(w, r, "serviceAccountId")
	if !ok {
		return
	}
	if sa.activeCredentials(o, now) >= activeCredentialsMax {
		recordedConflict(w, "conflict: service account \""+sa.id+"\" already has 5 active credentials; "+
			"delete one before creating another", "")
		return
	}

	// A fixed prefix, as the run's own secrets have, so the acceptance
	// suite's search of every log finds one that leaked.
	secret := issuedSecretPrefix + randomHexString(24)
	c := &fakeCredential{
		id: "cred-" + randomHexString(16), account: sa.id, secretHash: sha256.Sum256([]byte(secret)),
		expiresAt: now.Add(credentialLifetime), createdAt: now,
	}
	if body.ExpiresAt != nil {
		c.expiresAt = body.ExpiresAt.UTC()
	}
	credentialKind.put(o, sa.id, c.id, c)
	answer := c.body(sa.org, now)
	answer["clientSecret"] = secret
	writeJSON(w, http.StatusCreated, answer)
}

// credentialOf finds the credential a request's path names under an account in
// the request's organization. The caller holds s.mu.
func (s *state) credentialOf(w http.ResponseWriter, r *http.Request) (*objects, *fakeServiceAccount, *fakeCredential, at, bool) {
	o, sa, _, ok := s.accountInOrg(w, r, "serviceAccountId")
	if !ok {
		return nil, nil, nil, at{}, false
	}
	c, where, ok := credentialKind.find(w, r, o)
	return o, sa, c, where, ok
}

func (s *state) getServiceAccountCredential(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, sa, c, _, ok := s.credentialOf(w, r); ok {
		writeJSON(w, http.StatusOK, c.body(sa.org, time.Now()))
	}
}

func (s *state) deleteServiceAccountCredential(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, _, c, where, ok := s.credentialOf(w, r); ok {
		credentialKind.tearDown(o, where, c)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *state) listServiceAccountCredentials(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if credentialList.refusesMixed(w, q) {
		return
	}
	status := q.Get("status")
	if status != "" && status != "active" && status != "expired" {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "status "+status+" is not one of active, expired")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, sa, _, ok := s.accountInOrg(w, r, "serviceAccountId")
	if !ok {
		return
	}
	now := time.Now()
	items := []any{}
	for _, c := range credentialKind.under(o, sa.id) {
		row := c.body(sa.org, now)
		if status == "" || row["status"] == status {
			items = append(items, row)
		}
	}
	credentialList.serve(w, r, items)
}

// serviceAccountMints is the token endpoint's check for a service account: a
// client id that names an active account, and a secret that is one of its
// credentials' and hasn't expired. A disabled account mints nothing, as the
// contract says.
func (s *state) serviceAccountMints(clientID, secret string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.scopes[scope{}]
	if !ok {
		return false
	}
	now := time.Now()
	hash := sha256.Sum256([]byte(secret))
	for _, sa := range serviceAccountKind.under(o, "") {
		if !same(clientID, sa.clientID()) || sa.status != "active" {
			continue
		}
		for _, c := range credentialKind.under(o, sa.id) {
			if !c.expired(now) && subtle.ConstantTimeCompare(hash[:], c.secretHash[:]) == 1 {
				return true
			}
		}
	}
	return false
}

// nullIfEmpty is how dev.4 answered an unset description: null, not "".
func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func randomHexString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
