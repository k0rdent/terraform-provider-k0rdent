package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fake listens on loopback, and loopback isn't private on a machine more
// than one person uses: any local account can connect, and so can a page in
// any browser on it, either directly or through a DNS name that rebinds to
// 127.0.0.1. These tests hold the three lines that keep them out. The Host
// header has to name this listener, a request a browser made is refused, and
// nothing is served without a secret made for this run alone.

const testPort = "54321"

func guardedFake(t *testing.T) (http.Handler, runSecrets) {
	t.Helper()
	return guardedFakeOf(t, newState())
}

// guardedFakeOf is guardedFake serving s, for a test that reaches past the
// routes into what the fake holds.
func guardedFakeOf(t *testing.T, s *state) (http.Handler, runSecrets) {
	t.Helper()
	secrets, err := newRunSecrets()
	if err != nil {
		t.Fatalf("newRunSecrets: %v", err)
	}
	return newHandler(s, secrets, testPort, func() {}), secrets
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func mintRequest(host, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/regions/global/auth/token", strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	return req
}

func clientGrant(secret string) string {
	return grantJSON(map[string]string{
		"grant_type": "client_credentials", "client_id": wantClientID, "client_secret": secret,
	})
}

func grantJSON(fields map[string]string) string {
	b, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestFakeServesOnlyItsOwnHost(t *testing.T) {
	h, secrets := guardedFake(t)

	for _, host := range []string{"127.0.0.1:" + testPort, "localhost:" + testPort, "[::1]:" + testPort} {
		if rec := serve(h, mintRequest(host, clientGrant(secrets.ClientSecret))); rec.Code != http.StatusOK {
			t.Errorf("Host %s: mint answered %d, want 200: %s", host, rec.Code, rec.Body)
		}
	}
	// A rebinding page reaches 127.0.0.1 under its own name, and a request
	// for another port or with no port at all isn't for this listener.
	for _, host := range []string{
		"attacker.example:" + testPort, "attacker.example", "127.0.0.1", "127.0.0.1:1",
		"localhost.attacker.example:" + testPort, "",
	} {
		if rec := serve(h, mintRequest(host, clientGrant(secrets.ClientSecret))); rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: mint answered %d, want 403", host, rec.Code)
		}
	}
}

func TestFakeRefusesABrowser(t *testing.T) {
	h, secrets := guardedFake(t)
	// A form post or a no-cors fetch needs no preflight, so the header the
	// browser can't leave off is what tells it apart from Terraform.
	for name, value := range map[string]string{
		"Origin":         "http://attacker.example",
		"Sec-Fetch-Mode": "no-cors",
		"Sec-Fetch-Site": "cross-site",
	} {
		req := mintRequest("127.0.0.1:"+testPort, clientGrant(secrets.ClientSecret))
		req.Header.Set(name, value)
		if rec := serve(h, req); rec.Code != http.StatusForbidden {
			t.Errorf("a request carrying %s answered %d, want 403", name, rec.Code)
		}
	}
}

func TestFakeMintsOnlyWithThisRunsSecrets(t *testing.T) {
	h, secrets := guardedFake(t)
	host := "127.0.0.1:" + testPort

	// The secrets this repository used to publish as constants.
	for _, stale := range []string{
		clientGrant("k0r_fake_secret_for_local_verification_only"),
		clientGrant("k0r_fake_secret_after_rotation"),
		`{"grant_type":"api_key","api_key":"k0r_fake_api_key_for_local_verification_only"}`,
	} {
		if rec := serve(h, mintRequest(host, stale)); rec.Code != http.StatusUnauthorized {
			t.Errorf("a published constant minted (%d): %s", rec.Code, stale)
		}
	}

	key := grantJSON(map[string]string{"grant_type": "api_key", "api_key": secrets.APIKey})
	for _, good := range []string{clientGrant(secrets.ClientSecret), clientGrant(secrets.ClientSecretNext), key} {
		rec := serve(h, mintRequest(host, good))
		if rec.Code != http.StatusOK {
			t.Fatalf("this run's secret did not mint (%d): %s", rec.Code, rec.Body)
		}
		var got struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.AccessToken != secrets.AccessToken {
			t.Errorf("minted %q, want this run's access token", got.AccessToken)
		}
	}
}

func TestFakeAcceptsOnlyThisRunsBearer(t *testing.T) {
	h, secrets := guardedFake(t)
	for bearer, want := range map[string]int{
		"fake-access-token":       http.StatusUnauthorized, // the old constant
		secrets.AccessToken:       http.StatusOK,
		secrets.OwnerToken:        http.StatusUnauthorized,
		secrets.ClientSecret:      http.StatusUnauthorized,
		secrets.AccessToken + "x": http.StatusUnauthorized,
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/regions", nil)
		req.Host = "localhost:" + testPort
		req.Header.Set("Authorization", "Bearer "+bearer)
		if rec := serve(h, req); rec.Code != want {
			t.Errorf("GET /v1/regions with bearer %.24q answered %d, want %d", bearer, rec.Code, want)
		}
	}
}

func TestFakeHarnessRoutesNeedTheOwnerToken(t *testing.T) {
	h, secrets := guardedFake(t)
	for bearer, want := range map[string]int{
		secrets.OwnerToken:  http.StatusNoContent,
		secrets.AccessToken: http.StatusUnauthorized,
		"":                  http.StatusUnauthorized,
	} {
		req := httptest.NewRequest(http.MethodGet, "/_harness/ready", nil)
		req.Host = "127.0.0.1:" + testPort
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if rec := serve(h, req); rec.Code != want {
			t.Errorf("/_harness/ready answered %d, want %d", rec.Code, want)
		}
	}
	// The demo's old routes, keyed on a token passed in argv, are gone.
	req := httptest.NewRequest(http.MethodGet, "/_demo/healthz", nil)
	req.Host = "127.0.0.1:" + testPort
	if rec := serve(h, req); rec.Code != http.StatusNotFound {
		t.Errorf("/_demo/healthz answered %d, want 404", rec.Code)
	}
}

func TestRunSecretsAreFreshEachRun(t *testing.T) {
	a, err := newRunSecrets()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newRunSecrets()
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{a.ClientSecret, b.ClientSecret}, {a.ClientSecretNext, b.ClientSecretNext},
		{a.APIKey, b.APIKey}, {a.AccessToken, b.AccessToken}, {a.OwnerToken, b.OwnerToken},
	} {
		if pair[0] == pair[1] {
			t.Errorf("two runs made the same secret %q", pair[0])
		}
	}
	// The prefixes stay, so a log search for a fake credential still finds one.
	for value, prefix := range map[string]string{
		a.ClientSecret: "k0r_fake_secret_", a.ClientSecretNext: "k0r_fake_secret_after_rotation_",
		a.APIKey: "k0r_fake_api_key_", a.AccessToken: "fake-access-token-",
	} {
		if !strings.HasPrefix(value, prefix) || len(value) < len(prefix)+32 {
			t.Errorf("%q does not start %q followed by at least 32 random characters", value, prefix)
		}
	}
}

func TestSecretsFileIsPrivateAndNeverReused(t *testing.T) {
	secrets, err := newRunSecrets()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	if err = secrets.write(path); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the secrets file is %v, want -rw-------", mode)
	}

	// A file already there may be someone else's, or a symlink to one. Either
	// way it isn't written through.
	if err = secrets.write(path); err == nil {
		t.Error("write replaced a file that was already there")
	}
	target := filepath.Join(dir, "elsewhere")
	link := filepath.Join(dir, "planted")
	if err = os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err = secrets.write(link); err == nil {
		t.Error("write followed a planted symlink")
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Error("a planted symlink's target was created")
	}

	// It's read by sourcing it, so it has to be shell that sets exactly these.
	out, err := exec.Command("bash", "-c", `set -eu; . "$1"; printf '%s\n' "$FAKE_CLIENT_ID" "$FAKE_CLIENT_SECRET" "$FAKE_CLIENT_SECRET_NEXT" "$FAKE_API_KEY" "$FAKE_OWNER_TOKEN"`, "bash", path).Output()
	if err != nil {
		t.Fatalf("sourcing the secrets file: %v", err)
	}
	want := strings.Join([]string{wantClientID, secrets.ClientSecret, secrets.ClientSecretNext, secrets.APIKey, secrets.OwnerToken}, "\n") + "\n"
	if string(out) != want {
		t.Errorf("sourcing the secrets file gave\n%s\nwant\n%s", out, want)
	}
}
