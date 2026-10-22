// Command statebackend serves Terraform's `http` state backend protocol from
// memory, on loopback, so the conformance suite can run Terraform against
// remote state without a cloud account or a container.
//
// The provider has to work against remote state and not depend on state
// held on the machine that runs it. Proving that needs state that lives
// somewhere other than the working directory, and a second working directory
// that has never seen the first. This process is the "somewhere other": it
// holds each state document in memory under the path it was written to, so
// nothing either working directory writes can stand in for it.
//
// The protocol is Terraform's own (developer.hashicorp.com/terraform/language/backend/http):
//
//	GET    /<name>   the state, or 404 when none has been written
//	POST   /<name>   replace the state with the request body
//	DELETE /<name>   forget the state
//	LOCK   /<name>   take the lock, or 423 with the holder's lock info
//	UNLOCK /<name>   release the lock
//
// Locking is implemented rather than skipped, because a backend that cannot
// lock is not what a pipeline would point at, and a run that takes and releases
// the lock is evidence that the second working directory went through the same
// path the first one did.
//
// Each request is logged to stderr as "<time> METHOD /path", one per line, so
// a run can show which working directory wrote and read the state. The
// address it listens on is printed to stdout and nothing else is.
//
// Loopback isn't private on a machine more than one person uses, so three
// things hold the state to the run that started this. The Host header has to
// name this listener, which a page that rebinds a DNS name to 127.0.0.1 can't
// do. A request carrying the headers only a browser sends is refused. And
// every request needs HTTP basic auth with a password made for this run and
// written, 0600, to the file -password-file names. Terraform's http backend
// sends it from TF_HTTP_PASSWORD, so it's never in a configuration file.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type store struct {
	mu     sync.Mutex
	states map[string][]byte
	locks  map[string][]byte
}

func (s *store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.URL.Path)
	name := r.URL.Path
	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		body, ok := s.states[name]
		if !ok {
			http.Error(w, "no state", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeBody(w, body)
	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !json.Valid(body) {
			http.Error(w, "state is not JSON", http.StatusBadRequest)
			return
		}
		s.states[name] = body
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(s.states, name)
		w.WriteHeader(http.StatusOK)
	case "LOCK":
		if held, ok := s.locks[name]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusLocked)
			writeBody(w, held)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.locks[name] = body
		w.WriteHeader(http.StatusOK)
	case "UNLOCK":
		delete(s.locks, name)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// writeBody sends a response body. A write that fails means the client went
// away, which is logged rather than answered.
func writeBody(w http.ResponseWriter, body []byte) {
	if _, err := w.Write(body); err != nil {
		log.Printf("statebackend: write response: %v", err)
	}
}

// newPassword makes this run's password.
func newPassword() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// writePassword puts the password in a file the harness sources. It's created
// 0600 and never opened if it already exists: a file already there may be
// another user's, or a symlink they planted to read what's written. The
// password is hex, so single quotes make the line plain shell.
func writePassword(path, password string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "STATE_BACKEND_PASSWORD='%s'\n", password)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// newHandler is the backend behind its guard: this listener's Host only, no
// browsers, and this run's password on every request. port is the listener's.
func newHandler(port, password string) http.Handler {
	s := &store{states: map[string][]byte{}, locks: map[string][]byte{}}
	allowed := map[string]bool{}
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		allowed[net.JoinHostPort(host, port)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "this backend serves only its own loopback address", http.StatusForbidden)
			return
		}
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Mode") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "this backend does not serve browsers", http.StatusForbidden)
			return
		}
		// Compared in constant time, so a wrong guess learns nothing from how
		// long the answer took. The username is whatever Terraform was given.
		if _, got, ok := r.BasicAuth(); !ok || subtle.ConstantTimeCompare([]byte(got), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="statebackend"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address; port 0 picks a free one")
	passwordFile := flag.String("password-file", "", "write this run's password here, 0600; must not exist yet")
	flag.Parse()

	// Loopback only. State holds every attribute the provider reads back, and
	// this process serves it to anyone who asks.
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("statebackend: -addr %q is not host:port: %v", *addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		log.Fatalf("statebackend: -addr %q is not a loopback address", *addr)
	}

	if *passwordFile == "" {
		log.Fatalf("statebackend: -password-file is required. Each run makes its own password " +
			"and writes it there for the harness to read.")
	}
	password, err := newPassword()
	if err != nil {
		log.Fatalf("statebackend: %v", err)
	}
	if err = writePassword(*passwordFile, password); err != nil {
		log.Fatalf("statebackend: write the password file: %v", err)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("statebackend: listen: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcpAddr.IP.IsLoopback() {
		_ = ln.Close()
		log.Fatalf("statebackend: refusing non-loopback listener %q", ln.Addr())
	}
	log.SetFlags(log.Ldate | log.Ltime)
	log.SetOutput(os.Stderr)
	fmt.Printf("http://%s\n", ln.Addr())

	srv := &http.Server{Handler: newHandler(strconv.Itoa(tcpAddr.Port), password), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil {
		log.Fatalf("statebackend: %v", err)
	}
}
