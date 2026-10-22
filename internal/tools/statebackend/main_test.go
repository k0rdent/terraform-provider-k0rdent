package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The state this backend holds is every attribute the provider read back, and
// it listens where every account on the machine, and every browser page, can
// reach it. These hold it to the run that started it: the Host header has to
// name this listener, a browser's request is refused, and nothing is read or
// written without the password made for this run.

const testPort = "54322"

func backend(t *testing.T) (http.Handler, string) {
	t.Helper()
	password, err := newPassword()
	if err != nil {
		t.Fatalf("newPassword: %v", err)
	}
	return newHandler(testPort, password), password
}

func request(method, host, user, password, body string) *http.Request {
	req := httptest.NewRequest(method, "/conformance", strings.NewReader(body))
	req.Host = host
	if user != "" || password != "" {
		req.SetBasicAuth(user, password)
	}
	return req
}

func code(h http.Handler, req *http.Request) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestBackendNeedsThisRunsPassword(t *testing.T) {
	h, password := backend(t)
	host := "127.0.0.1:" + testPort

	for name, req := range map[string]*http.Request{
		"no credential":   request(http.MethodPost, host, "", "", `{}`),
		"wrong password":  request(http.MethodPost, host, "terraform", password+"x", `{}`),
		"empty password":  request(http.MethodPost, host, "terraform", "", `{}`),
		"read, no secret": request(http.MethodGet, host, "", "", ""),
		"lock, no secret": request("LOCK", host, "", "", `{}`),
	} {
		if got := code(h, req); got != http.StatusUnauthorized {
			t.Errorf("%s: answered %d, want 401", name, got)
		}
	}

	if got := code(h, request(http.MethodPost, host, "terraform", password, `{"serial":1}`)); got != http.StatusOK {
		t.Fatalf("a write with this run's password answered %d, want 200", got)
	}
	if got := code(h, request(http.MethodGet, host, "terraform", password, "")); got != http.StatusOK {
		t.Errorf("a read with this run's password answered %d, want 200", got)
	}
}

func TestBackendServesOnlyItsOwnHost(t *testing.T) {
	h, password := backend(t)
	for _, host := range []string{"127.0.0.1:" + testPort, "localhost:" + testPort, "[::1]:" + testPort} {
		if got := code(h, request(http.MethodGet, host, "terraform", password, "")); got != http.StatusNotFound {
			t.Errorf("Host %s: answered %d, want 404 for state not yet written", host, got)
		}
	}
	for _, host := range []string{"attacker.example:" + testPort, "127.0.0.1", "127.0.0.1:1", ""} {
		if got := code(h, request(http.MethodGet, host, "terraform", password, "")); got != http.StatusForbidden {
			t.Errorf("Host %q: answered %d, want 403", host, got)
		}
	}
}

func TestBackendRefusesABrowser(t *testing.T) {
	h, password := backend(t)
	for _, header := range []string{"Origin", "Sec-Fetch-Mode", "Sec-Fetch-Site"} {
		req := request(http.MethodGet, "127.0.0.1:"+testPort, "terraform", password, "")
		req.Header.Set(header, "x")
		if got := code(h, req); got != http.StatusForbidden {
			t.Errorf("a request carrying %s answered %d, want 403", header, got)
		}
	}
}

func TestPasswordFileIsPrivateAndNeverReused(t *testing.T) {
	a, err := newPassword()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newPassword()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || len(a) < 32 {
		t.Errorf("passwords %q and %q are not fresh and long", a, b)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "password")
	if err = writePassword(path, a); err != nil {
		t.Fatalf("writePassword: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the password file is %v, want -rw-------", mode)
	}
	if err = writePassword(path, b); err == nil {
		t.Error("writePassword replaced a file that was already there")
	}
	link := filepath.Join(dir, "planted")
	if err = os.Symlink(filepath.Join(dir, "elsewhere"), link); err != nil {
		t.Fatal(err)
	}
	if err = writePassword(link, b); err == nil {
		t.Error("writePassword followed a planted symlink")
	}
	out, err := exec.Command("bash", "-c", `set -eu; . "$1"; printf '%s' "$STATE_BACKEND_PASSWORD"`, "bash", path).Output()
	if err != nil {
		t.Fatalf("sourcing the password file: %v", err)
	}
	if string(out) != a {
		t.Errorf("sourcing the password file gave %q, want %q", out, a)
	}
}
