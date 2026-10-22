package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// org is the path an account route takes, with more path values added.
func org(more map[string]string) map[string]string {
	out := map[string]string{"orgId": "org-demo"}
	for k, v := range more {
		out[k] = v
	}
	return out
}

func createAccount(t *testing.T, s *state, body string) map[string]any {
	t.Helper()
	code, out := in(t, s.createServiceAccount, http.MethodPost, "", "", org(nil), body)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	return out
}

// An account is made as the contract and dev.4 describe it: the id it was sent or one
// it generates, both under the `sa-` prefix, a client id of its own, `active`,
// and its roles as given.
func TestAServiceAccountIsMadeAsTheContractSays(t *testing.T) {
	s := newState()
	got := createAccount(t, s, `{"id":"sa-pipeline","displayName":"Pipeline","scope":"project","scopeId":"demo","roles":["compute-admin","compute-admin"]}`)
	if got["clientId"] != "sa-pipeline@demo.iam" || got["status"] != "active" || got["scope"] != "project" {
		t.Errorf("create answered %v", got)
	}
	if roles := field[[]any](t, got, "roles"); len(roles) != 1 || roles[0] != "compute-admin" {
		t.Errorf("roles = %v, want the one slug once", got["roles"])
	}

	generated := createAccount(t, s, `{"displayName":"Generated","scope":"organization","scopeId":"org-demo"}`)
	if id := field[string](t, generated, "id"); !serviceAccountIDPattern.MatchString(id) {
		t.Errorf("a generated id %q doesn't match the create's own pattern", id)
	}
	if roles := field[[]any](t, generated, "roles"); len(roles) != 0 {
		t.Errorf("an account created with no roles answers %v, want []", generated["roles"])
	}

	for name, body := range map[string]string{
		"an id without the prefix":          `{"id":"pipeline","displayName":"x","scope":"project","scopeId":"demo"}`,
		"a scope outside the enum":          `{"displayName":"x","scope":"cluster","scopeId":"demo"}`,
		"a project the organization lacks":  `{"displayName":"x","scope":"project","scopeId":"nowhere"}`,
		"another organization's id":         `{"displayName":"x","scope":"organization","scopeId":"org-empty"}`,
		"a role that doesn't exist":         `{"displayName":"x","scope":"project","scopeId":"demo","roles":["compute.deployer"]}`,
		"an empty display name":             `{"displayName":"","scope":"project","scopeId":"demo"}`,
		"a description past 1024 character": `{"displayName":"x","description":"` + strings.Repeat("d", 1025) + `","scope":"project","scopeId":"demo"}`,
	} {
		if status, out := in(t, s.createServiceAccount, http.MethodPost, "", "", org(nil), body); status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %v, want 422", name, status, out)
		}
	}
	if status, _ := in(t, s.createServiceAccount, http.MethodPost, "", "", org(nil),
		`{"id":"sa-pipeline","displayName":"again","scope":"project","scopeId":"demo"}`); status != http.StatusConflict {
		t.Errorf("a taken id: %d, want 409", status)
	}
}

// A PATCH sets what it names, a delete is permanent at once, and an account in
// another organization is the 404 a missing one is.
func TestAServiceAccountChangesInPlaceAndIsDeletedAtOnce(t *testing.T) {
	s := newState()
	createAccount(t, s, `{"id":"sa-ci","displayName":"CI","scope":"project","scopeId":"demo","roles":["compute-admin"]}`)
	id := org(map[string]string{"id": "sa-ci"})

	code, out := in(t, s.updateServiceAccount, http.MethodPatch, "", "", id, `{"roles":[],"status":"disabled","description":""}`)
	if code != http.StatusOK || out["status"] != "disabled" || out["description"] != nil {
		t.Fatalf("patch: %d %v", code, out)
	}
	if roles := field[[]any](t, out, "roles"); len(roles) != 0 {
		t.Errorf("roles after clearing them: %v", out["roles"])
	}
	if status, _ := in(t, s.updateServiceAccount, http.MethodPatch, "", "", id, `{}`); status != http.StatusBadRequest {
		t.Errorf("an empty patch: %d, want 400", status)
	}

	other := map[string]string{"orgId": "org-empty", "id": "sa-ci"}
	if status, answer := in(t, s.getServiceAccount, http.MethodGet, "", "", other, ""); status != http.StatusNotFound {
		t.Errorf("from another organization: %d %v", status, answer)
	}

	if status, _ := in(t, s.deleteServiceAccount, http.MethodDelete, "", "", id, ""); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	code, out = in(t, s.getServiceAccount, http.MethodGet, "", "", id, "")
	if code != http.StatusNotFound || !strings.Contains(field[string](t, field[map[string]any](t, out, "error"), "message"), `service account "sa-ci"`) {
		t.Errorf("read after the delete: %d %v", code, out)
	}
}

// The list needs both `scope` and `scopeId`, as the contract requires, and
// answers only what matches them.
func TestServiceAccountsAreListedByScope(t *testing.T) {
	s := newState()
	createAccount(t, s, `{"id":"sa-a","displayName":"a","scope":"project","scopeId":"demo"}`)
	createAccount(t, s, `{"id":"sa-b","displayName":"b","scope":"organization","scopeId":"org-demo"}`)
	for query, want := range map[string]int{"": 422, "scope=project": 422, "scope=project&scopeId=demo": 200} {
		req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		req.SetPathValue("orgId", "org-demo")
		rec := httptest.NewRecorder()
		s.listServiceAccounts(rec, req)
		if rec.Code != want {
			t.Errorf("?%s: %d, want %d", query, rec.Code, want)
			continue
		}
		if want == 200 {
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || listed(out, "serviceAccounts") != 1 {
				t.Errorf("?%s: %s", query, rec.Body)
			}
		}
	}
}

// The secret is in the create's answer and in nothing after it. A sixth live
// credential is the 409, an expiry in the past the 422, and one left out
// lasts 90 days.
func TestACredentialsSecretIsAnsweredOnce(t *testing.T) {
	s := newState()
	createAccount(t, s, `{"id":"sa-ci","displayName":"CI","scope":"project","scopeId":"demo"}`)
	under := org(map[string]string{"serviceAccountId": "sa-ci"})

	code, made := in(t, s.createServiceAccountCredential, http.MethodPost, "", "", under, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, made)
	}
	secret := field[string](t, made, "clientSecret")
	id := field[string](t, made, "id")
	if !strings.HasPrefix(secret, "k0r_") || !strings.HasPrefix(id, "cred-") || len(id) != len("cred-")+32 {
		t.Fatalf("create answered %v", made)
	}
	expires, err := time.Parse(time.RFC3339, field[string](t, made, "expiresAt"))
	if err != nil || time.Until(expires) < 89*24*time.Hour {
		t.Errorf("expiresAt %v, want about 90 days out", made["expiresAt"])
	}

	code, read := in(t, s.getServiceAccountCredential, http.MethodGet, "", "", org(map[string]string{"serviceAccountId": "sa-ci", "id": id}), "")
	if code != http.StatusOK {
		t.Fatalf("read: %d %v", code, read)
	}
	if _, has := read["clientSecret"]; has {
		t.Errorf("a read carries the secret: %v", read)
	}
	_, list := in(t, s.listServiceAccountCredentials, http.MethodGet, "", "", under, "")
	if strings.Contains(mustJSON(t, list), secret) {
		t.Errorf("the list carries the secret: %v", list)
	}

	for range activeCredentialsMax - 1 {
		if status, out := in(t, s.createServiceAccountCredential, http.MethodPost, "", "", under, `{}`); status != http.StatusCreated {
			t.Fatalf("create: %d %v", status, out)
		}
	}
	if status, _ := in(t, s.createServiceAccountCredential, http.MethodPost, "", "", under, `{}`); status != http.StatusConflict {
		t.Errorf("a sixth: %d, want 409", status)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if status, _ := in(t, s.createServiceAccountCredential, http.MethodPost, "", "", under, `{"expiresAt":"`+past+`"}`); status != http.StatusUnprocessableEntity {
		t.Errorf("an expiry in the past: %d, want 422", status)
	}

	missing := org(map[string]string{"serviceAccountId": "sa-gone", "id": id})
	code, out := in(t, s.getServiceAccountCredential, http.MethodGet, "", "", missing, "")
	if code != http.StatusNotFound || !strings.Contains(mustJSON(t, out), `service account \"sa-gone\"`) {
		t.Errorf("a credential under a missing account: %d %v, want the account's 404", code, out)
	}
}

// The secret authenticates for as long as the platform says it should: not
// once its account is disabled, and not once the credential is deleted.
func TestAServiceAccountCredentialMints(t *testing.T) {
	s := newState()
	createAccount(t, s, `{"id":"sa-ci","displayName":"CI","scope":"project","scopeId":"demo"}`)
	under := org(map[string]string{"serviceAccountId": "sa-ci"})
	_, made := in(t, s.createServiceAccountCredential, http.MethodPost, "", "", under, `{}`)
	secret, id := field[string](t, made, "clientSecret"), field[string](t, made, "id")

	if !s.serviceAccountMints("sa-ci@demo.iam", secret) {
		t.Fatal("the issued secret doesn't mint")
	}
	if s.serviceAccountMints("sa-ci@demo.iam", secret+"x") || s.serviceAccountMints("sa-other@org-demo.iam", secret) {
		t.Error("a wrong secret or client id mints")
	}
	in(t, s.updateServiceAccount, http.MethodPatch, "", "", org(map[string]string{"id": "sa-ci"}), `{"status":"disabled"}`)
	if s.serviceAccountMints("sa-ci@demo.iam", secret) {
		t.Error("a disabled account mints")
	}
	in(t, s.updateServiceAccount, http.MethodPatch, "", "", org(map[string]string{"id": "sa-ci"}), `{"status":"active"}`)
	in(t, s.deleteServiceAccountCredential, http.MethodDelete, "", "", org(map[string]string{"serviceAccountId": "sa-ci", "id": id}), "")
	if s.serviceAccountMints("sa-ci@demo.iam", secret) {
		t.Error("a deleted credential mints")
	}

	rs := runSecrets{AccessToken: "tok"}
	rec := httptest.NewRecorder()
	_, made = in(t, s.createServiceAccountCredential, http.MethodPost, "", "", under, `{}`)
	body := `{"grant_type":"client_credentials","client_id":"sa-ci@demo.iam","client_secret":"` + field[string](t, made, "clientSecret") + `"}`
	rs.minting(s.serviceAccountMints)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Errorf("the token endpoint refused a live credential: %d %s", rec.Code, rec.Body)
	}
}

// Deleting an account takes its credentials with it, so none is left to read
// or to mint with.
func TestDeletingAServiceAccountTakesItsCredentials(t *testing.T) {
	s := newState()
	createAccount(t, s, `{"id":"sa-ci","displayName":"CI","scope":"project","scopeId":"demo"}`)
	_, made := in(t, s.createServiceAccountCredential, http.MethodPost, "", "", org(map[string]string{"serviceAccountId": "sa-ci"}), `{}`)
	in(t, s.deleteServiceAccount, http.MethodDelete, "", "", org(map[string]string{"id": "sa-ci"}), "")
	if n := len(s.scopes[scope{}].credentials); n != 0 {
		t.Errorf("%d credentials outlived their account", n)
	}
	if s.serviceAccountMints("sa-ci@demo.iam", field[string](t, made, "clientSecret")) {
		t.Error("a deleted account's credential mints")
	}
}

// A credential has no id the marker can ride in, so it churns through its
// account's, in the two fields a pipeline's use of it moves.
func TestAMarkedAccountsCredentialChurns(t *testing.T) {
	body := []byte(`{"id":"cred-0","serviceAccountId":"sa-` + churnsOnEveryRead + `","status":"active"}`)
	odd, ok := churnBody(body, 1)
	if !ok || !strings.Contains(string(odd), `"lastUsedAt"`) {
		t.Fatalf("an odd read: %s", odd)
	}
	even, ok := churnBody(body, 2)
	if !ok || strings.Contains(string(even), `"lastUsedAt"`) {
		t.Errorf("an even read: %s", even)
	}
	if strings.Contains(string(odd), `"updatedAt"`) {
		t.Errorf("a credential gained a field its contract doesn't declare: %s", odd)
	}
}

// field is m[key] as a T, failing the test when it's anything else.
func field[T any](t *testing.T, m map[string]any, key string) T {
	t.Helper()
	v, ok := m[key].(T)
	if !ok {
		t.Fatalf("%s is %#v, not a %T", key, m[key], v)
	}
	return v
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The roles a binding names are role ids, which listRoles answers.
func TestRolesAreListedByID(t *testing.T) {
	s := newState()
	code, out := in(t, s.listRoles, http.MethodGet, "", "", org(nil), "")
	if code != http.StatusOK || listed(out, "roles") == 0 {
		t.Fatalf("listRoles: %d %v", code, out)
	}
	for _, r := range field[[]any](t, out, "roles") {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("a role row is %#v", r)
		}
		id := field[string](t, row, "id")
		if !serviceAccountIDLikeRole.MatchString(id) {
			t.Errorf("role id %q doesn't match Role.id's pattern", id)
		}
	}
}

// serviceAccountIDLikeRole is Role.id's pattern from the contract.
var serviceAccountIDLikeRole = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
