package main

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"path"
	"strings"
	"time"
)

// Each SSH key a cluster or group is created with is registered as an
// `ssh_public_key` credential in its project, and a read of the credential
// answers the key's metadata, its SHA-256 fingerprint among it, never the
// key. As the k0rdent API v1.3.0-rc.5 answered on kind: an account with
// `credentials.metadata.read` reads it, one with `compute-admin` alone gets
// the gateway's bare 403, and a revoked credential reads `REVOKED` with its
// revocation as the operation. Refusal is per key, not per account, so a test
// can refuse its own key's reads without touching another test's.
type fakeSSHKey struct {
	fingerprint, keyType, comment string
	createdAt                     time.Time
	revokedAt                     *time.Time
	readsRefused                  bool
}

// registerSSHKey records the credential a key is registered as. The same key
// makes the same credential, so a second create keeps the first record.
func (o *objects) registerSSHKey(ref, key string) {
	id := path.Base(ref)
	if _, ok := o.sshKeys[id]; ok {
		return
	}
	f := strings.Fields(key)
	k := &fakeSSHKey{createdAt: time.Now().UTC().Truncate(time.Second)}
	if len(f) >= 2 {
		k.keyType = f[0]
		if blob, err := base64.StdEncoding.DecodeString(f[1]); err == nil {
			sum := sha256.Sum256(blob)
			k.fingerprint = "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
		}
	}
	if len(f) >= 3 {
		k.comment = strings.Join(f[2:], " ")
	}
	o.sshKeys[id] = k
}

func (s *state) getProjectCredential(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)
	id := r.PathValue("id")
	k, ok := o.sshKeys[id]
	if ok && k.readsRefused {
		writeBody(w, http.StatusForbidden, errorBody{Error: errorFields{
			Code: http.StatusForbidden, Message: "forbidden", RequestID: fakeRequestID,
		}})
		return
	}
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "credential "+id+" not found")
		return
	}
	body := map[string]any{
		"id":             id,
		"type":           "ssh_public_key",
		"scope":          "project",
		"projectId":      o.scope.project,
		"state":          "ACTIVE",
		"currentVersion": 1,
		"placement":      "region:" + o.scope.region,
		"createdAt":      k.createdAt.Format(time.RFC3339),
		"updatedAt":      k.createdAt.Format(time.RFC3339),
		"materialAttributes": map[string]string{
			"fingerprint": k.fingerprint, "keyType": k.keyType, "comment": k.comment,
		},
	}
	if k.revokedAt != nil {
		body["state"] = "REVOKED"
		body["updatedAt"] = k.revokedAt.Format(time.RFC3339)
		body["operation"] = map[string]any{
			"type": "revocation", "generation": 1, "status": "ok", "startedAt": k.revokedAt.Format(time.RFC3339),
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// sshKeyHarness acts on a key's credential as an operator or another account
// would: POST /_harness/regions/{region}/projects/{project}/credentials/{id}/
// followed by revoke (an operator revokes it in the console), refuse-reads
// (reads answer 403, as to an account with `compute-admin` alone) or
// open-reads.
func (s *state) sshKeyHarness(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.in(r).sshKeys[r.PathValue("id")]
	if !ok {
		http.Error(w, "no such SSH key credential", http.StatusNotFound)
		return
	}
	switch r.PathValue("action") {
	case "revoke":
		now := time.Now().UTC().Truncate(time.Second)
		k.revokedAt = &now
	case "refuse-reads":
		k.readsRefused = true
	case "open-reads":
		k.readsRefused = false
	default:
		http.Error(w, "want revoke, refuse-reads or open-reads", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
