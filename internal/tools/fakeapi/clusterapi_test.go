package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTheClusterAPIRefusesATokenOnceItExpires(t *testing.T) {
	a, err := startClusterAPI()
	if err != nil {
		t.Fatal(err)
	}
	a.ttl = 300 * time.Millisecond
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(a.caPEM) {
		t.Fatal("the CA is not PEM")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	get := func(token string) int {
		t.Helper()
		req, reqErr := http.NewRequest(http.MethodGet, a.url+"/clusters/c1/api", nil)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, doErr := client.Do(req)
		if doErr != nil {
			t.Fatal(doErr)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	jwt, opaque := a.mint(false), a.mint(true)
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("the JWT is %q", jwt)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !strings.Contains(string(payload), `"exp":`) || !strings.HasPrefix(parts[2], "not-a-real-token-") {
		t.Errorf("the JWT is %q", jwt)
	}
	if strings.Contains(opaque, ".") || !strings.HasPrefix(opaque, "not-a-real-token-") {
		t.Errorf("the opaque token is %q", opaque)
	}
	for _, tok := range []string{jwt, opaque} {
		if code := get(tok); code != http.StatusOK {
			t.Errorf("a live token got %d", code)
		}
	}
	if code := get("not-a-real-token-unknown"); code != http.StatusUnauthorized {
		t.Errorf("a token it never minted got %d", code)
	}
	time.Sleep(400 * time.Millisecond)
	for _, tok := range []string{jwt, opaque} {
		if code := get(tok); code != http.StatusUnauthorized {
			t.Errorf("an expired token got %d", code)
		}
	}
}
