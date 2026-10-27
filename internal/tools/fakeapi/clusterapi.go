package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Display-name markers. The usual kubeconfig's server doesn't exist. These get
// a real TLS server that refuses the token after expiringTokenTTL.
const (
	// A JWT whose `exp` is when the server starts refusing it, as the platform's is.
	kubeconfigExpires = "fake-kubeconfig-expires"
	// Opaque, so only a 401 says it expired.
	kubeconfigExpiresOpaque = "fake-kubeconfig-expires-opaque"
	// Only the second read answers 403, like revoked access. Destroy still reads it.
	kubeconfigReissueDenied = "fake-kubeconfig-reissue-denied"

	expiringTokenTTL = 5 * time.Second
)

// clusterAPI serves every expiring cluster, with the id in the path.
type clusterAPI struct {
	url   string
	caPEM []byte
	ttl   time.Duration // shorter in tests

	mu     sync.Mutex
	tokens map[string]time.Time
}

func startClusterAPI() (*clusterAPI, error) {
	caPEM, cert, err := clusterAPICertificates()
	if err != nil {
		return nil, err
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, err
	}
	a := &clusterAPI{url: "https://" + ln.Addr().String(), caPEM: caPEM, ttl: expiringTokenTTL, tokens: map[string]time.Time{}}
	srv := &http.Server{Handler: a, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("fakeapi: cluster API: %v", err)
		}
	}()
	return a, nil
}

// The `not-a-real-token` prefix is what the acceptance log gate searches for.
func (a *clusterAPI) mint(opaque bool) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	name := "not-a-real-token-" + hex.EncodeToString(b)
	expires := time.Now().Add(a.ttl)
	token := name
	if !opaque {
		enc := base64.RawURLEncoding.EncodeToString
		token = enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
			enc([]byte(fmt.Sprintf(`{"exp":%d,"aud":"fake-cluster"}`, expires.Unix()))) + "." + name
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[token] = expires
	return token
}

// Log lines omit the token, and `ua=` so the request audit skips them.
func (a *clusterAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	a.mu.Lock()
	expires, known := a.tokens[token]
	a.mu.Unlock()
	switch {
	case !known:
		log.Printf("cluster-api %s %s 401 unknown-token", r.Method, r.URL.EscapedPath())
		clusterStatus(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return
	case !time.Now().Before(expires):
		log.Printf("cluster-api %s %s 401 expired", r.Method, r.URL.EscapedPath())
		clusterStatus(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || (r.Method == http.MethodPost && len(body) == 0) {
		log.Printf("cluster-api %s %s 400 empty-body", r.Method, r.URL.EscapedPath())
		clusterStatus(w, http.StatusBadRequest, "BadRequest", "the request has no body")
		return
	}
	log.Printf("cluster-api %s %s 200", r.Method, r.URL.EscapedPath())
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Success","bodyBytes":%d}`, len(body))
}

// A 401's reason and message are "Unauthorized", as Kubernetes sends them.
// The relay reads that as a refused token.
func clusterStatus(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":%q,"reason":%q,"code":%d}`,
		message, reason, code)
}

// A private CA, as a real cluster has, so clients verify against the kubeconfig's CA.
func clusterAPICertificates() ([]byte, tls.Certificate, error) {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake cluster CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}, nil
}

func (s *state) expiringKubeconfig(c *cluster) (string, bool) {
	if s.clusterAPI == nil || !nameCarries(c.displayName, kubeconfigExpires) {
		return "", false
	}
	token := s.clusterAPI.mint(nameCarries(c.displayName, kubeconfigExpiresOpaque))
	return fmt.Sprintf(`apiVersion: v1
kind: Config
# NOT A REAL CREDENTIAL. Served by internal/tools/fakeapi for local
# verification. It authenticates against the fake's own cluster API only.
clusters:
  - name: %[1]s
    cluster:
      server: %[2]s/clusters/%[1]s
      certificate-authority-data: %[3]s
contexts:
  - name: %[1]s
    context:
      cluster: %[1]s
      user: fake-admin
current-context: %[1]s
users:
  - name: fake-admin
    user:
      token: %[4]s
`, c.id, s.clusterAPI.url, base64.StdEncoding.EncodeToString(s.clusterAPI.caPEM), token), true
}
