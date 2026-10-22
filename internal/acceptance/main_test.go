// Package acceptance drives real Terraform against the local fake.
//
// Separate from the unit tests because it needs a control plane and a terraform
// binary, and separate from `make demo` because the demo tells one story end to
// end while this covers each resource's own lifecycle: create, an empty second
// plan, update, both import forms, and destroy.
package acceptance

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/apiaudit"
	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var endpoint string

// fakeLogPath is the fake's request log for this run, for a test that has to
// see which requests the provider sent.
var fakeLogPath string

// fakeOwnerToken is the fake's owner token for this run. It reads the fake's
// own store, which the provider's credentials can't.
var fakeOwnerToken string

var protoV6 = map[string]func() (tfprotov6.ProviderServer, error){
	provider.ProviderTypeName: providerserver.NewProtocol6WithError(provider.New("acc")()),
}

func TestMain(m *testing.M) {
	// `-sweep` runs the sweepers against whatever API the environment names
	// and nothing else: no fake, no tests. terraform-plugin-testing's own
	// TestMain does that and exits. See sweep_test.go.
	flag.Parse()
	if sweeping() {
		resource.TestMain(m)
		return
	}
	if os.Getenv("TF_ACC") == "" {
		// Every resource.Test skips itself without it, so no control plane is
		// needed. The checks that do not want one still run.
		fmt.Println("acceptance: TF_ACC is not set, running only the checks that need no fake")
		os.Exit(m.Run())
	}
	// The fake never recovers a failed object, so waiting out a recovery only
	// slows the suite.
	provider.ProvisionalFailureWindow = 2 * time.Second
	// The fake serves an offset page at the size asked for, as the contract
	// says, so the provider asks for two rows and a handful of objects is
	// more than one page.
	provider.OffsetPageLimit = 2
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "acceptance:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	// One 0700 directory with an unguessable name, for the binary and the trace
	// log. A guessable path can be pre-created as a symlink, and this harness
	// writes a binary there, marks it executable and runs it.
	scratch, mkErr := os.MkdirTemp("", "k0rdent-acc-")
	if mkErr != nil {
		return 0, fmt.Errorf("making a scratch directory: %w", mkErr)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	// Built rather than `go run`, so the process we signal is the server itself
	// and not a wrapper that outlives it.
	bin := filepath.Join(scratch, "fakeapi")
	if out, err := exec.Command("go", "build", "-o", bin, "../tools/fakeapi").CombinedOutput(); err != nil {
		return 0, fmt.Errorf("building the fake: %w: %s", err, out)
	}

	// The fake makes this run's credentials and writes them here, 0600, inside
	// the 0700 scratch directory. Nothing else accepts them.
	secretsPath := filepath.Join(scratch, "fake-secrets.env")
	fake := exec.Command(bin, "-addr", "127.0.0.1:0", "-secrets-file", secretsPath)
	stdout, err := fake.StdoutPipe()
	if err != nil {
		return 0, err
	}
	// The fake's request log is kept as well as shown, so gate S4 searches it as
	// well as the trace log.
	fakeLogPath = filepath.Join(scratch, "fake.log")
	fakeLog, logErr := os.OpenFile(fakeLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if logErr != nil {
		return 0, fmt.Errorf("opening the fake's log: %w", logErr)
	}
	defer func() { _ = fakeLog.Close() }()
	fake.Stderr = io.MultiWriter(os.Stderr, fakeLog)
	if err = fake.Start(); err != nil {
		return 0, fmt.Errorf("starting the fake: %w", err)
	}
	defer func() {
		if kerr := fake.Process.Kill(); kerr != nil {
			fmt.Fprintln(os.Stderr, "acceptance: could not stop the fake:", kerr)
		}
		// Reaping, not checking: Kill above is what makes Wait return.
		if _, werr := fake.Process.Wait(); werr != nil {
			fmt.Fprintln(os.Stderr, "acceptance: reaping the fake:", werr)
		}
	}()

	// The fake prints its URL on the first line and then logs every request. The
	// rest has to be drained or it blocks on a full pipe and stops serving.
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("reading the fake's address: %w", err)
	}
	endpoint = strings.TrimSpace(line)
	go func() {
		if _, cerr := io.Copy(io.Discard, reader); cerr != nil {
			fmt.Fprintln(os.Stderr, "acceptance: stopped draining the fake's output:", cerr)
		}
	}()

	if err = waitForFake(endpoint); err != nil {
		return 0, err
	}
	secrets, err := readFakeSecrets(secretsPath)
	if err != nil {
		return 0, err
	}
	fakeOwnerToken = secrets["FAKE_OWNER_TOKEN"]

	for k, v := range map[string]string{
		provider.EnvVar("ENDPOINT"):      endpoint,
		provider.EnvVar("CLIENT_ID"):     secrets["FAKE_CLIENT_ID"],
		provider.EnvVar("CLIENT_SECRET"): secrets["FAKE_CLIENT_SECRET"],
		provider.EnvVar("REGION"):        "us-texas-1",
		provider.EnvVar("PROJECT"):       "demo",
		// Loopback is exempt from the one-second floor. At the 10s default
		// every wait sleeps against a fake that settles in two reads.
		provider.EnvVar("POLL_INTERVAL"): "100ms",
	} {
		if err := os.Setenv(k, v); err != nil {
			return 0, err
		}
	}
	// TRACE is the level that carries request and response bodies, and the
	// token mint's body is the credential. Captured so the run can be searched
	// afterwards rather than trusted.
	logPath := filepath.Join(scratch, "trace.log")
	for k, v := range map[string]string{"TF_LOG": "trace", "TF_ACC_LOG_PATH": logPath} {
		if err := os.Setenv(k, v); err != nil {
			return 0, err
		}
	}
	code := m.Run()

	// Checked whether or not the suite passed: a leak is likeliest on an error
	// path, where messages get formatted.
	gateErr := assertNoSecretInLog(logPath, fakeLogPath)
	switch {
	case gateErr == nil:
	case errors.Is(gateErr, errNothingSearched):
		// Nothing reached either log. Under `-run` that is an honest "no
		// acceptance test executed" and not a finding. If the fake saw
		// traffic, Terraform did run and a missing trace log is a broken
		// harness rather than a quiet pass.
		if info, serr := os.Stat(fakeLogPath); serr == nil && info.Size() > 0 {
			fmt.Fprintln(os.Stderr, "acceptance: the fake served requests but no log was searchable")
			return 1, nil
		}
		fmt.Fprintln(os.Stderr, "acceptance: gate S4 had nothing to search, no acceptance test ran")
	default:
		fmt.Fprintln(os.Stderr, "acceptance:", gateErr)
		return 1, nil
	}
	if !providerSentOnlyPublicRequests(fakeLogPath) {
		return 1, nil
	}
	return code, nil
}

// providerSentOnlyPublicRequests reads every request in the fake's log but
// this harness's own and requires each to be an operation the contract marks
// public. The provider is tenant-facing, and this is the run-time evidence for
// that, as opposed to a lint rule that only says how the code was meant to be
// written. It covers what this suite drove and no other code path.
func providerSentOnlyPublicRequests(fakeLogPath string) bool {
	body, err := os.ReadFile(fakeLogPath) //nolint:gosec // a path this harness made
	if err != nil {
		fmt.Fprintln(os.Stderr, "acceptance: could not read the fake's log:", err)
		return false
	}
	return auditVerdict(apiaudit.AuditProviderRequests(string(body)))
}

// auditVerdict is whether the audit passed, printing why. It's split out so
// the vacuous case can be tested without a run.
func auditVerdict(audit apiaudit.RequestAudit) bool {
	switch {
	case audit.Requests == 0 && audit.Harness == 0:
		// The same reasoning as gate S4: under `-run` nothing may have reached
		// the fake, which is not a finding.
		fmt.Fprintln(os.Stderr, "acceptance: the fake served no request, so there was nothing to check for visibility")
		return true
	case audit.Requests == 0:
		// The harness reached the fake and the provider didn't, or the audit
		// stopped seeing it. Either way it proved nothing, so it isn't a pass.
		fmt.Fprintf(os.Stderr, "acceptance: the fake served %d harness requests and none the audit could check, so it proves nothing\n", audit.Harness)
		return false
	}
	if err := audit.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "acceptance:", err)
		return false
	}
	fmt.Fprintf(os.Stderr, "acceptance: %s: %s\n", audit, strings.Join(audit.Public, ", "))
	return true
}

// secretNeedles are the values gate S4 searches for. `not-a-real-token` is the
// only one with a producer today: it is the kubeconfig body, which crosses the
// plugin protocol through the ephemeral resource. The others are kept as a
// tripwire for the day a logging transport or a wider fake log gives them one.
// The fake makes its secrets for each run, behind fixed prefixes, so the
// prefixes find one whichever run wrote it.
func secretNeedles() []string {
	return []string{
		os.Getenv(provider.EnvVar("CLIENT_SECRET")),
		"fake-access-token", // the prefix of the bearer the fake mints
		"not-a-real-token",  // the credential inside the fake's kubeconfig
		"k0r_fake_secret_",  // the prefix of the fake's client secrets
		"k0r_fake_api_key_", // the prefix of the fake's API key
		// Every webhook signing secret this suite configures. Write-only
		// means Terraform never stores one, and this is where a log would
		// show it had.
		webhookSecretPrefix,
		// The prefix of every secret a service account credential is issued.
		// The provider seals it before state sees it, and nothing should ever
		// write it down in the clear.
		"k0r_fake_sa_secret_",
	}
}

// readFakeSecrets reads the file the fake wrote its credentials to: one
// NAME='value' line each, values hex behind a fixed prefix, so no shell is
// needed to read it.
func readFakeSecrets(path string) (map[string]string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // a path this harness made
	if err != nil {
		return nil, fmt.Errorf("reading the fake's secrets: %w", err)
	}
	secrets := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(name, "#") {
			continue
		}
		secrets[name] = strings.Trim(value, "'")
	}
	for _, name := range []string{"FAKE_CLIENT_ID", "FAKE_CLIENT_SECRET", "FAKE_OWNER_TOKEN"} {
		if secrets[name] == "" {
			return nil, fmt.Errorf("the fake's secrets file has no %s", name)
		}
	}
	return secrets, nil
}

// errNothingSearched means every named log was absent or empty, so the gate
// proves nothing. Whether that is a failure depends on whether anything ran,
// which only the caller knows.
var errNothingSearched = errors.New("gate S4 searched no logs, so it proves nothing")

// assertNoSecretInLog is gate S4: no credential at any log level, TRACE
// included. Redaction covers diagnostics, and a diagnostic is not the only
// thing written down.
func assertNoSecretInLog(paths ...string) error {
	scanned := 0
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("could not read the log at %s: %w", path, err)
		}
		if len(body) == 0 {
			continue
		}
		scanned++
		for _, secret := range secretNeedles() {
			if secret == "" {
				continue
			}
			if bytes.Contains(body, []byte(secret)) {
				return fmt.Errorf("the credential %q appears in %s", secret, path)
			}
		}
		if bytes.Contains(body, []byte("Authorization: Bearer ")) {
			return fmt.Errorf("an Authorization header value appears in %s", path)
		}
	}
	if scanned == 0 {
		return errNothingSearched
	}
	return nil
}

// harnessHTTP is how this suite calls the fake itself, for setup and to read
// what the provider left behind. It sends apiaudit.HarnessUserAgent, which is
// the one thing the request audit leaves out, so every other request in the
// fake's log is checked as the provider's.
var harnessHTTP = &http.Client{Transport: harnessTransport{base: provider.NewPlatformTransport()}}

// The harness has a pool of its own, as the provider does. On
// http.DefaultTransport, the httptest.Server.Close in state_migration_test.go
// would close this suite's idle connections under a request that had just
// picked one up.

// harnessTransport stamps apiaudit.HarnessUserAgent on every request this
// suite sends to the fake directly.
type harnessTransport struct{ base http.RoundTripper }

func (t harnessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", apiaudit.HarnessUserAgent)
	return t.base.RoundTrip(req)
}

func waitForFake(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("the fake printed %q, which is not a URL: %w", endpoint, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", u.Host, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the fake never accepted a connection on %s", u.Host)
}

// Gate S4's check has to be able to fail: a grep with no producer is not a
// gate. This plants each needle in a file and asserts the checker reports it,
// so the checker's own behaviour is verified whether or not a real run ever
// produces one.
// The visibility audit has to refuse a run where the fake saw traffic and
// none of it was checked, as gate S4 refuses a run it couldn't search.
func TestAuditVerdictRefusesAVacuousRun(t *testing.T) {
	harness := `2026/10/10 12:00:00 GET /_harness/ready ua="` + apiaudit.HarnessUserAgent + `" auth=present`
	if auditVerdict(apiaudit.AuditProviderRequests(harness)) {
		t.Error("a log of only harness requests passed")
	}
	if !auditVerdict(apiaudit.AuditProviderRequests("")) {
		t.Error("an empty log, which is what `-run` with no acceptance test leaves, failed")
	}
	handBuilt := `2026/10/10 12:00:00 POST /v1/regions/global/organizations ua="Go-http-client/1.1" auth=present`
	if auditVerdict(apiaudit.AuditProviderRequests(harness + "\n" + handBuilt)) {
		t.Error("a hand-built createOrganization passed")
	}
	public := `2026/10/10 12:00:00 GET /v1/regions/us-texas-1/compute/cluster-types ua="terraform-provider-k0rdent/acc" auth=present`
	if !auditVerdict(apiaudit.AuditProviderRequests(harness + "\n" + public)) {
		t.Error("a public provider request beside a harness one failed")
	}
}

func TestGateS4CanFail(t *testing.T) {
	t.Setenv(provider.EnvVar("CLIENT_SECRET"), "planted-client-secret")

	for _, needle := range []string{
		"planted-client-secret",
		"fake-access-token-0123",
		"not-a-real-token",
		"k0r_fake_secret_0123",
		"k0r_fake_api_key_0123",
		"Authorization: Bearer abc123",
	} {
		path := filepath.Join(t.TempDir(), "log")
		body := "a line before\n" + needle + "\na line after\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing the fixture: %v", err)
		}
		if err := assertNoSecretInLog(path); err == nil {
			t.Errorf("gate S4 passed a log containing %q", needle)
		}
	}

	// And it still passes a log with nothing in it.
	clean := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(clean, []byte("nothing to see\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	if err := assertNoSecretInLog(clean); err != nil {
		t.Errorf("gate S4 failed a clean log: %v", err)
	}

	// An empty log proves nothing and must not read as a pass.
	empty := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	if err := assertNoSecretInLog(empty); err == nil {
		t.Error("gate S4 passed an empty log")
	}
	if err := assertNoSecretInLog(); err == nil {
		t.Error("gate S4 passed with no logs at all")
	}
}
