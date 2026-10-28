package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Who may drive this process.
//
// Loopback isn't private on a machine more than one person uses. Any local
// account can connect to 127.0.0.1, and so can a page open in any browser on
// the machine: directly, with a form post or a no-cors fetch that needs no
// preflight, or through a DNS name that rebinds to 127.0.0.1. The fake mints
// tokens and holds whatever a run created, so three things stand in the way.
//
//  1. The Host header has to name this listener. A rebinding page reaches the
//     socket under its own name, and that name is refused.
//  2. A request that carries the headers only a browser sends is refused.
//  3. Nothing is served without a secret made for this run. The credentials
//     used to be constants in this repository, which every local user can
//     read. Now each run makes its own, writes them to a file only its owner
//     can read, and the harness that started it reads them from there.

// wantClientID is the service account the fake knows. An id, not a secret,
// so it stays fixed: storage.go records it as the creator of what it makes.
const wantClientID = "sa-terraform-provider@vendor.iam"

// projectClientID is a second service account, bound to one project and
// nothing above it: compute-admin on the seeded project `demo` in `org-demo`.
// Its token is refused what the k0rdent API v1.3.0-rc.3 refuses such a
// credential, so a test can show what one can and can't read. See
// scopedToProject.
const projectClientID = "sa-terraform-provider-project@demo.iam"

// projectPrincipal is where projectClientID's one role is bound.
var projectPrincipal = principal{org: "org-demo", project: "demo"}

// principal is the scope a request's credential is bound to. The zero value
// is the run's own credential, which the fake scopes by nothing.
type principal struct{ org, project string }

type principalKey struct{}

// scopedTo is the principal a request's token was minted for.
func scopedTo(r *http.Request) principal {
	if p, ok := r.Context().Value(principalKey{}).(principal); ok {
		return p
	}
	return principal{}
}

// sees is whether the principal's lists show a project in an organization.
// The run's own credential sees everything.
func (p principal) sees(org, project string) bool {
	return p.project == "" || (p.org == org && p.project == project)
}

// seesOrganization is whether the principal's organization list shows org. A
// project's organization is listed to a credential bound to the project.
func (p principal) seesOrganization(org string) bool {
	return p.project == "" || p.org == org
}

// scopedToProject refuses, with the gateway's bare 403, what a credential
// bound to one project can't reach, and reports whether it did. As the
// k0rdent API v1.3.0-rc.3 answered such a credential:
//
//   - Every route under /organizations/{orgId} is checked against the
//     organization, and a role bound to a project never counts there. The
//     organization, the project by id, both quotas, roles and service
//     accounts were each 403 `forbidden` with no reason.
//   - A route under another project's /projects/{project} finds no role.
//
// The flat lists aren't refused. They show only what the principal sees:
// listProjects its own project, listOrganizations its project's organization.
func scopedToProject(w http.ResponseWriter, r *http.Request) bool {
	p := scopedTo(r)
	if p.project == "" {
		return false
	}
	org, project := r.PathValue("orgId"), r.PathValue("project")
	if org == "" && (project == "" || project == p.project) {
		return false
	}
	writeBody(w, http.StatusForbidden, errorBody{Error: errorFields{
		Code: http.StatusForbidden, Message: "forbidden", RequestID: fakeRequestID,
	}})
	return true
}

// runSecrets are one run's credentials. There are two client secrets for one
// client id because a service account can hold several credentials at once,
// and that overlap is what makes rotation zero-downtime. The conformance suite
// rotates mid-run to show that nothing in Terraform state is bound to which
// credential authenticated.
type runSecrets struct {
	ClientSecret     string
	ClientSecretNext string
	APIKey           string
	// AccessToken is what a mint hands out and every API route requires.
	AccessToken string
	// ProjectClientSecret is projectClientID's secret, and
	// ProjectAccessToken what it mints.
	ProjectClientSecret string
	ProjectAccessToken  string
	// OwnerToken drives /_harness/ready and /_harness/shutdown, so the
	// script that started the fake can tell it's still its own, and stop it.
	// It also opens /_harness/store, the read past the gateway that lets a
	// destroy check see an object whose project is gone, which every API
	// route answers with the gateway's 403.
	OwnerToken string
}

// newRunSecrets makes a fresh set. Each keeps a recognisable prefix, so a
// search of a log for a leaked fake credential still finds one by its prefix.
func newRunSecrets() (runSecrets, error) {
	var rs runSecrets
	for _, f := range []struct {
		into   *string
		prefix string
	}{
		{&rs.ClientSecret, "k0r_fake_secret_"},
		{&rs.ClientSecretNext, "k0r_fake_secret_after_rotation_"},
		{&rs.APIKey, "k0r_fake_api_key_"},
		{&rs.AccessToken, "fake-access-token-"},
		{&rs.ProjectClientSecret, "k0r_fake_secret_project_"},
		{&rs.ProjectAccessToken, "fake-access-token-project-"},
		{&rs.OwnerToken, ""},
	} {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return runSecrets{}, fmt.Errorf("read random bytes: %w", err)
		}
		*f.into = f.prefix + hex.EncodeToString(b[:])
	}
	return rs, nil
}

// write puts the secrets a client needs in a file the harness sources. It's
// created 0600 and never opened if it already exists: a file already there
// may be another user's, or a symlink they planted to read what's written.
// Every value is hex behind a fixed prefix, so single quotes are enough to
// make each line plain shell.
func (rs runSecrets) write(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, `# Written by internal/tools/fakeapi for one run. Only the fake accepts these.
FAKE_CLIENT_ID='%s'
FAKE_CLIENT_SECRET='%s'
FAKE_CLIENT_SECRET_NEXT='%s'
FAKE_API_KEY='%s'
FAKE_OWNER_TOKEN='%s'
FAKE_PROJECT_CLIENT_ID='%s'
FAKE_PROJECT_CLIENT_SECRET='%s'
`, wantClientID, rs.ClientSecret, rs.ClientSecretNext, rs.APIKey, rs.OwnerToken,
		projectClientID, rs.ProjectClientSecret)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// same compares in constant time, so a wrong guess learns nothing from how
// long the answer took.
func same(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// guard refuses a request for any host but this listener, and any request a
// browser made, before a route sees it. port is the listener's own.
func guard(port string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		allowed[net.JoinHostPort(host, port)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "this fake serves only its own loopback address", http.StatusForbidden)
			return
		}
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Mode") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "this fake does not serve browsers", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// minting is the anonymous global-tier mint. It accepts both machine grants
// with this run's secrets, and, when account is set, a service account
// credential the fake issued, so a test can show a secret it was handed
// authenticates. A token minted either way is the run's one access token,
// which the fake scopes by nothing. projectClientID is the one exception: it
// mints its own token, scoped to its project.
func (rs runSecrets) minting(account func(clientID, secret string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { rs.mintWith(account, w, r) }
}

func (rs runSecrets) mintWith(account func(clientID, secret string) bool, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		// The token endpoint is anonymous. A bearer header here would mean the
		// client tried to authenticate its own token request, which is the
		// recursion the thin layer exists to avoid.
		apiError(w, http.StatusBadRequest, "INVALID_REQUEST", "the mint must be anonymous")
		return
	}

	var grant map[string]any
	if err := json.NewDecoder(r.Body).Decode(&grant); err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_REQUEST", "the mint body is not JSON")
		return
	}
	str := func(key string) string {
		if s, ok := grant[key].(string); ok {
			return s
		}
		return ""
	}

	token := rs.AccessToken
	switch grant["grant_type"] {
	case "client_credentials":
		secret := str("client_secret")
		run := same(str("client_id"), wantClientID) &&
			(same(secret, rs.ClientSecret) || same(secret, rs.ClientSecretNext))
		if same(str("client_id"), projectClientID) && same(secret, rs.ProjectClientSecret) {
			run, token = true, rs.ProjectAccessToken
		}
		if !run && (account == nil || !account(str("client_id"), secret)) {
			apiError(w, http.StatusUnauthorized, "INVALID_CREDENTIAL", "unknown client id or secret")
			return
		}
	case "api_key":
		// Compared, not prefix-tested: a prefix test mints a token for any
		// string starting `k0r_`, so a wrong-key test could never fail.
		if !same(str("api_key"), rs.APIKey) {
			apiError(w, http.StatusUnauthorized, "INVALID_CREDENTIAL", "unknown api key")
			return
		}
	default:
		apiError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("unsupported grant_type %v", grant["grant_type"]))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int((15 * time.Minute).Seconds()),
	})
}

// auth wraps a handler in the bearer-token check every non-mint route has.
// The project credential's token carries its principal to the gateway.
func (rs runSecrets) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		switch {
		case same(got, "Bearer "+rs.AccessToken):
			next(w, r)
		case same(got, "Bearer "+rs.ProjectAccessToken):
			next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, projectPrincipal)))
		default:
			apiError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "missing or unrecognised bearer token")
		}
	}
}

// ownerControl answers the harness's readiness and shutdown requests, for the
// owner token only.
func (rs runSecrets) ownerControl(shutdown func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !same(r.Header.Get("Authorization"), "Bearer "+rs.OwnerToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if shutdown != nil {
			w.WriteHeader(http.StatusAccepted)
			shutdown()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// owner serves next for the owner token only, like ownerControl.
func (rs runSecrets) owner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !same(r.Header.Get("Authorization"), "Bearer "+rs.OwnerToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
