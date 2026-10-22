package provider

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// envMap turns a map into the lookup function resolveSettings takes, so a
// table row can describe an environment without touching the process's.
func envMap(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// saEnv is a complete service account credential, for the many cases that need
// resolution to get past the credential check to reach what they are testing.
func saEnv(extra map[string]string) map[string]string {
	out := map[string]string{
		EnvVar(envClientID):     "sa-terraform@vendor.iam",
		EnvVar(envClientSecret): "k0r_secret",
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// Precedence
// ---------------------------------------------------------------------------

func TestPrecedenceBlockBeatsEnvBeatsDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block blockValues
		env   map[string]string
		want  Settings
	}{
		{
			name:  "nothing set at all: only endpoint has a default",
			block: blockValues{},
			env:   saEnv(nil),
			want:  Settings{Endpoint: DefaultEndpoint},
		},
		{
			name:  "environment alone",
			block: blockValues{},
			env: saEnv(map[string]string{
				EnvVar(envEndpoint): "https://api.dev02.example.com",
				EnvVar(envRegion):   "us-texas-1",
				EnvVar(envProject):  "cp-production",
			}),
			want: Settings{
				Endpoint: "https://api.dev02.example.com",
				Region:   "us-texas-1",
				Project:  "cp-production",
			},
		},
		{
			name: "block alone",
			block: blockValues{
				Endpoint: "https://api.dev01.example.com",
				Region:   "eu-west-1",
				Project:  "cp-example",
			},
			env: saEnv(nil),
			want: Settings{
				Endpoint: "https://api.dev01.example.com",
				Region:   "eu-west-1",
				Project:  "cp-example",
			},
		},
		{
			name: "block wins over environment, attribute by attribute",
			block: blockValues{
				Endpoint: "https://from-block.example.com",
				Region:   "region-from-block",
			},
			env: saEnv(map[string]string{
				EnvVar(envEndpoint): "https://from-env.example.com",
				EnvVar(envRegion):   "region-from-env",
				EnvVar(envProject):  "project-from-env",
			}),
			want: Settings{
				Endpoint: "https://from-block.example.com",
				Region:   "region-from-block",
				// Not set in the block, so the environment still supplies it.
				Project: "project-from-env",
			},
		},
		{
			name:  "environment wins over the endpoint default",
			block: blockValues{},
			env:   saEnv(map[string]string{EnvVar(envEndpoint): "https://from-env.example.com"}),
			want:  Settings{Endpoint: "https://from-env.example.com"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, diags := resolveSettings(tc.block, envMap(tc.env))
			if diags.HasError() {
				t.Fatalf("unexpected errors: %v", diags.Errors())
			}
			if got.Endpoint != tc.want.Endpoint {
				t.Errorf("Endpoint = %q, want %q", got.Endpoint, tc.want.Endpoint)
			}
			if got.Region != tc.want.Region {
				t.Errorf("Region = %q, want %q", got.Region, tc.want.Region)
			}
			if got.Project != tc.want.Project {
				t.Errorf("Project = %q, want %q", got.Project, tc.want.Project)
			}
		})
	}
}

// An empty or whitespace-only value is treated as absent at every layer. This
// is the documented rule and it is the one most likely to surprise, because
// `region = var.region` with an unset variable produces exactly this.
func TestEmptyAndWhitespaceValuesFallThrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block blockValues
		env   map[string]string
		want  Settings
	}{
		{
			name:  "empty block attribute falls through to the environment",
			block: blockValues{Region: "", Endpoint: ""},
			env: saEnv(map[string]string{
				EnvVar(envRegion):   "us-texas-1",
				EnvVar(envEndpoint): "https://from-env.example.com",
			}),
			want: Settings{Endpoint: "https://from-env.example.com", Region: "us-texas-1"},
		},
		{
			name:  "whitespace-only block attribute falls through too",
			block: blockValues{Region: "   ", Endpoint: "\t\n"},
			env: saEnv(map[string]string{
				EnvVar(envRegion):   "us-texas-1",
				EnvVar(envEndpoint): "https://from-env.example.com",
			}),
			want: Settings{Endpoint: "https://from-env.example.com", Region: "us-texas-1"},
		},
		{
			name:  "empty environment variable falls through to the default",
			block: blockValues{},
			env:   saEnv(map[string]string{EnvVar(envEndpoint): "  "}),
			want:  Settings{Endpoint: DefaultEndpoint},
		},
		{
			name:  "surrounding whitespace is trimmed, not preserved",
			block: blockValues{Region: "  us-texas-1  ", Project: " cp-production "},
			env:   saEnv(nil),
			want:  Settings{Endpoint: DefaultEndpoint, Region: "us-texas-1", Project: "cp-production"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, diags := resolveSettings(tc.block, envMap(tc.env))
			if diags.HasError() {
				t.Fatalf("unexpected errors: %v", diags.Errors())
			}
			if got.Endpoint != tc.want.Endpoint || got.Region != tc.want.Region || got.Project != tc.want.Project {
				t.Errorf("got endpoint=%q region=%q project=%q, want endpoint=%q region=%q project=%q",
					got.Endpoint, got.Region, got.Project,
					tc.want.Endpoint, tc.want.Region, tc.want.Project)
			}
		})
	}
}

// A secret must never be reachable from the provider block. The block reaches
// resolution as blockValues, so every field it has is listed here by name, and
// a field added later fails this until someone decides it carries no secret.
// The resolution half below can't see a new field, because it never sets one.
func TestBlockValuesCarryNoSecret(t *testing.T) {
	t.Parallel()

	notSecret := map[string]bool{
		"Endpoint": true, "Region": true, "Project": true,
		"OperationTimeout": true, "PollInterval": true,
	}
	fields := reflect.TypeFor[blockValues]()
	for i := range fields.NumField() {
		if name := fields.Field(i).Name; !notSecret[name] {
			t.Errorf("blockValues has a field %q that isn't on the list of fields that carry no "+
				"secret. A credential must come from the environment, never the provider block. "+
				"If %q carries none, add it to the list.", name, name)
		}
	}

	block := blockValues{
		Endpoint: "https://api.example.com",
		Region:   "us-texas-1",
		Project:  "cp-production",
	}
	got, diags := resolveSettings(block, envMap(saEnv(nil)))
	if diags.HasError() {
		t.Fatalf("unexpected errors: %v", diags.Errors())
	}
	if got.Credential.ClientSecret != "k0r_secret" {
		t.Fatalf("the credential did not come from the environment: %+v", got.Credential)
	}

	// Same block, empty environment: no credential at all, which is only
	// possible if nothing in the block can supply one.
	_, diags = resolveSettings(block, envMap(nil))
	if !diags.HasError() {
		t.Fatal("resolving with an empty environment produced no error; the block supplied a credential")
	}
}

// ---------------------------------------------------------------------------
// Targeting
// ---------------------------------------------------------------------------

// Since v1.3.0-dev.4 the region list says which regions run compute and hold
// projects, and on an all-in-one install `global` does. Whether it does here
// is the platform's to say, so the provider block takes it.
func TestRegionTakesTheControlPlaneTier(t *testing.T) {
	t.Parallel()

	s, diags := resolveSettings(blockValues{Region: globalRegion}, envMap(saEnv(nil)))
	if diags.HasError() {
		t.Fatalf("region %q was refused: %v", globalRegion, diags.Errors())
	}
	if s.Region != globalRegion {
		t.Errorf("region = %q", s.Region)
	}
}

// A project created without an id gets one the platform generates, and it
// starts `proj-`. Refusing that prefix made such a project unusable here.
func TestProjectAcceptsAGeneratedID(t *testing.T) {
	t.Parallel()

	if d := validateProject("proj-a1b2c3d4"); d.HasError() {
		t.Fatalf("a generated project id was refused: %v", d.Errors())
	}
	got, diags := resolveSettings(blockValues{Project: "proj-a1b2c3d4"}, envMap(saEnv(nil)))
	if diags.HasError() {
		t.Fatalf("a generated project id was refused: %v", diags.Errors())
	}
	if got.Project != "proj-a1b2c3d4" {
		t.Errorf("project = %q", got.Project)
	}
}

func TestProjectAcceptsASlug(t *testing.T) {
	t.Parallel()

	got, diags := resolveSettings(blockValues{Project: "cp-production"}, envMap(saEnv(nil)))
	if diags.HasError() {
		t.Fatalf("a slug was rejected: %v", diags.Errors())
	}
	if got.Project != "cp-production" {
		t.Fatalf("Project = %q, want %q", got.Project, "cp-production")
	}
}

func TestEndpointValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		endpoint string
		accepted bool
		summary  string
	}{
		{endpoint: DefaultEndpoint, accepted: true},
		{endpoint: "http://127.0.0.1:8080", accepted: true},
		{endpoint: "https://api.dev02.example.com", accepted: true},
		// A landscape served under a base path is legitimate. Only a version
		// segment is rejected, not any path.
		{endpoint: "https://gateway.example.com/k0rdent", accepted: true},

		{endpoint: "api.k0rdent.example.com", summary: "Invalid API endpoint"},
		{endpoint: "ftp://api.example.com", summary: "Invalid API endpoint"},
		{endpoint: "/v1/regions", summary: "Invalid API endpoint"},
		{endpoint: "https://", summary: "Invalid API endpoint"},
		{endpoint: "https://api.example.com?region=x", summary: "Invalid API endpoint"},
		{endpoint: "https://api.example.com#frag", summary: "Invalid API endpoint"},

		// The configuration layer supplies an origin; the generated client
		// supplies every path, including the version. An endpoint carrying one
		// sends it twice.
		{endpoint: "https://api.k0rdent.ai/v1", summary: "API endpoint includes a version segment"},
		{endpoint: "https://api.k0rdent.ai/v1/", summary: "API endpoint includes a version segment"},
		{endpoint: "https://api.k0rdent.ai/v2", summary: "API endpoint includes a version segment"},
		{endpoint: "https://gateway.example.com/k0rdent/v1", summary: "API endpoint includes a version segment"},
	}

	for _, tc := range tests {
		t.Run(tc.endpoint, func(t *testing.T) {
			t.Parallel()

			_, diags := resolveSettings(blockValues{Endpoint: tc.endpoint}, envMap(saEnv(nil)))

			if tc.accepted {
				if diags.HasError() {
					t.Fatalf("endpoint %q was rejected: %v", tc.endpoint, diags.Errors())
				}
				return
			}
			if !diags.HasError() {
				t.Fatalf("endpoint %q was accepted", tc.endpoint)
			}
			if !containsSummary(diags.Errors(), tc.summary) {
				t.Fatalf("endpoint %q: expected %q, got %v", tc.endpoint, tc.summary, diags.Errors())
			}
		})
	}
}

// The provider never assembles a request URL — every path comes from the
// generated client's operations. That is worth pinning rather than trusting,
// because the contract has demonstrated it moves paths without a deprecation
// window: nine public paths were removed between v1.0.1 and v1.2.0, the whole
// compute/networks family relocating to networking/networks in one release
// A path restated here would go stale with nothing to catch it.
//
// The only paths this package names are help text, and each is routed through a
// single constant so a move is one edit. Scanning every non-test file in the
// package rather than a named list means a new resource cannot quietly
// reintroduce the habit.
func TestTheProviderRestatesNoAPIPath(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// Only the constants defined for the purpose may spell a path out.
	allowed := []string{"regionsOperation =", "exampleClusterTypeRef ="}

	scanned := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		scanned++
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for i, line := range strings.Split(string(source), "\n") {
			if !strings.Contains(line, "/v1") || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if slices.ContainsFunc(allowed, func(a string) bool { return strings.Contains(line, a) }) {
				continue
			}
			t.Errorf("%s:%d writes an API path out; use a generated client operation, or one of "+
				"the help-text constants in config.go:\n  %s", file, i+1, strings.TrimSpace(line))
		}
	}
	if scanned < 5 {
		t.Fatalf("only scanned %d files; the glob is not finding the package", scanned)
	}
}

func TestRequireRegionAndProjectExplainHowToSetThem(t *testing.T) {
	t.Parallel()

	empty := &Meta{}

	if _, diags := empty.RequireRegion(); !diags.HasError() {
		t.Error("RequireRegion returned no error with no region configured")
	} else if !strings.Contains(diags.Errors()[0].Detail(), EnvVar(envRegion)) {
		t.Errorf("the region error does not name %s: %s", EnvVar(envRegion), diags.Errors()[0].Detail())
	}

	if _, diags := empty.RequireProject(); !diags.HasError() {
		t.Error("RequireProject returned no error with no project configured")
	} else if !strings.Contains(diags.Errors()[0].Detail(), EnvVar(envProject)) {
		t.Errorf("the project error does not name %s: %s", EnvVar(envProject), diags.Errors()[0].Detail())
	}

	set := &Meta{Region: "us-texas-1", Project: "cp-production"}
	if region, diags := set.RequireRegion(); diags.HasError() || region != "us-texas-1" {
		t.Errorf("RequireRegion() = %q, %v", region, diags.Errors())
	}
	if project, diags := set.RequireProject(); diags.HasError() || project != "cp-production" {
		t.Errorf("RequireProject() = %q, %v", project, diags.Errors())
	}
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

func TestCredentialResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		env         map[string]string
		wantKind    CredentialKind
		wantErr     string
		wantWarning bool
	}{
		{
			name: "service account",
			env: map[string]string{
				EnvVar(envClientID):     "sa-terraform@vendor.iam",
				EnvVar(envClientSecret): "k0r_secret",
			},
			wantKind: ServiceAccountCredential,
		},
		{
			name:        "api key resolves but warns",
			env:         map[string]string{EnvVar(envAPIKey): "k0r_apikey"},
			wantKind:    APIKeyCredential,
			wantWarning: true,
		},
		{
			name:    "no credential at all",
			env:     map[string]string{},
			wantErr: "No credential",
		},
		{
			name:    "client id without a secret",
			env:     map[string]string{EnvVar(envClientID): "sa-terraform@vendor.iam"},
			wantErr: "Incomplete service account credential",
		},
		{
			name:    "secret without a client id",
			env:     map[string]string{EnvVar(envClientSecret): "k0r_secret"},
			wantErr: "Incomplete service account credential",
		},
		{
			name: "both credential types is an error, not a guess",
			env: map[string]string{
				EnvVar(envClientID):     "sa-terraform@vendor.iam",
				EnvVar(envClientSecret): "k0r_secret",
				EnvVar(envAPIKey):       "k0r_apikey",
			},
			wantErr: "Ambiguous credentials",
		},
		{
			name: "a whitespace-only secret is not a secret",
			env: map[string]string{
				EnvVar(envClientID):     "sa-terraform@vendor.iam",
				EnvVar(envClientSecret): "   ",
			},
			wantErr: "Incomplete service account credential",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, diags := resolveSettings(blockValues{}, envMap(tc.env))

			if tc.wantErr != "" {
				if !diags.HasError() {
					t.Fatalf("expected error %q, got none", tc.wantErr)
				}
				if !containsSummary(diags.Errors(), tc.wantErr) {
					t.Fatalf("expected error %q, got %v", tc.wantErr, diags.Errors())
				}
				return
			}

			if diags.HasError() {
				t.Fatalf("unexpected errors: %v", diags.Errors())
			}
			if got.Credential.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Credential.Kind, tc.wantKind)
			}
			if tc.wantWarning && diags.WarningsCount() == 0 {
				t.Error("expected a warning, got none")
			}
			if !tc.wantWarning && diags.WarningsCount() != 0 {
				t.Errorf("unexpected warnings: %v", diags.Warnings())
			}
			if got.Credential.build() == nil {
				t.Error("build() returned a nil Credential")
			}
		})
	}
}

// The no-credential message is the first thing most people will see. It has to
// say what to set and that the provider cannot fix it for them, because
// provisioning is a human operation the platform denies to machine principals.
func TestNoCredentialErrorIsActionable(t *testing.T) {
	t.Parallel()

	_, diags := resolveSettings(blockValues{}, envMap(nil))
	detail := diags.Errors()[0].Detail()

	for _, want := range []string{EnvVar(envClientID), EnvVar(envClientSecret), "tenant administrator"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the no-credential error does not mention %q:\n%s", want, detail)
		}
	}
}

// ---------------------------------------------------------------------------
// Derived names
// ---------------------------------------------------------------------------

// Every environment variable name is built from ProviderTypeName, so that
// settling the name moves them with everything else rather than leaving a
// second, silent naming decision behind.
func TestEnvironmentVariableNamesDeriveFromTheConstant(t *testing.T) {
	t.Parallel()

	prefix := EnvPrefix()
	if !strings.HasPrefix(prefix, strings.ToUpper(ProviderTypeName)) {
		t.Fatalf("EnvPrefix() = %q, which is not derived from ProviderTypeName %q", prefix, ProviderTypeName)
	}
	if !strings.HasSuffix(prefix, "_") {
		t.Fatalf("EnvPrefix() = %q, want a trailing underscore", prefix)
	}

	for _, suffix := range []string{envEndpoint, envRegion, envProject, envClientID, envClientSecret, envAPIKey} {
		name := EnvVar(suffix)
		if !strings.HasPrefix(name, prefix) {
			t.Errorf("EnvVar(%q) = %q, which does not start with %q", suffix, name, prefix)
		}
		for _, r := range name {
			ok := (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
			if !ok {
				t.Errorf("EnvVar(%q) = %q contains %q, which is not valid in a shell identifier", suffix, name, r)
			}
		}
	}
}

func TestUserAgentDerivesFromTheConstant(t *testing.T) {
	t.Parallel()

	if got, want := UserAgent("1.2.3", "", ""), "terraform-provider-"+ProviderTypeName+"/1.2.3"; got != want {
		t.Fatalf("UserAgent() = %q, want %q", got, want)
	}
	if got := UserAgent("", "", ""); !strings.HasSuffix(got, "/dev") {
		t.Fatalf("UserAgent(\"\") = %q, want an unversioned build to say so", got)
	}
	if got, want := UserAgent("1.2.3", "1.9.5", ""),
		"Terraform/1.9.5 (+https://www.terraform.io) terraform-provider-"+ProviderTypeName+"/1.2.3"; got != want {
		t.Fatalf("UserAgent() = %q, want %q", got, want)
	}
}

func containsSummary[E interface{ Summary() string }](diags []E, summary string) bool {
	for _, d := range diags {
		if d.Summary() == summary {
			return true
		}
	}
	return false
}

func TestCleartextIsRejectedExceptToLoopback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		endpoint string
		wantErr  bool
	}{
		{"https://api.k0rdent.example", false},
		{"http://127.0.0.1:8080", false},
		{"http://localhost:8080", false},
		{"http://[::1]:8080", false},
		{"http://api.k0rdent.example", true},
		{"http://10.0.0.5", true},
	} {
		got := validateEndpoint(tc.endpoint).HasError()
		if got != tc.wantErr {
			t.Errorf("validateEndpoint(%q) error = %v, want %v", tc.endpoint, got, tc.wantErr)
		}
	}
}

// A path parameter is percent-escaped by the generated client, so a slash
// cannot inject a segment: but a dot is not escaped and the request builder
// finishes with a reference resolution that removes dot segments. `region=".."`
// drops `/regions/<r>` from the URL entirely.
func TestScopeRejectsAValueThatRewritesTheURL(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"..", ".", "../..", "us-west/../global", "us-west?x=1", "us-west#f", "US-WEST", "-leading"} {
		if d := validateRegion(bad); !d.HasError() {
			t.Errorf("validateRegion(%q) accepted it", bad)
		}
		if d := validateProject(bad); !d.HasError() {
			t.Errorf("validateProject(%q) accepted it", bad)
		}
	}
	for _, ok := range []string{"us-texas-1", "cp-production", "a", "a1", "global"} {
		if d := validateRegion(ok); d.HasError() {
			t.Errorf("validateRegion(%q) rejected it: %v", ok, d.Errors())
		}
	}
}

// `1ns` parses and is positive, and waitFor has no backoff. The floor applies
// to a remote endpoint and not to the local fake, which the demo and the
// conformance run both poll at 200ms on purpose.
func TestPollIntervalHasAFloorOnlyAgainstARemoteEndpoint(t *testing.T) {
	t.Parallel()

	// resolveSettings always reports the missing credential, so look for this
	// diagnostic by name rather than asking whether anything failed.
	refused := func(s Settings, d diag.Diagnostics) bool {
		for _, e := range d.Errors() {
			if e.Summary() == "Poll interval too short" {
				return true
			}
		}
		return false
	}

	if !refused(resolveSettings(blockValues{PollInterval: "1ns"}, func(string) string { return "" })) {
		t.Error("a 1ns poll interval was accepted against the default remote endpoint")
	}

	for _, local := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		if refused(resolveSettings(
			blockValues{Endpoint: local, PollInterval: "200ms"},
			func(string) string { return "" },
		)) {
			t.Errorf("200ms against the local fake at %s was refused", local)
		}
	}
}

// A kind stack serves the real platform on localhost, and its watchdog does
// recover a failed object, so a loopback endpoint gets the full window.
func TestProvisionalFailureWindowIsTheSameAgainstLoopback(t *testing.T) {
	for _, endpoint := range []string{"http://localhost:8000", "http://127.0.0.1:8080", "https://api.dev02.example.com"} {
		meta, err := newMeta(Settings{Endpoint: endpoint, Credential: CredentialSource{Kind: APIKeyCredential, APIKey: "k"}}, UserAgent("test", "", ""))
		if err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		if got := meta.Wait.ProvisionalFailureWindow; got != 2*time.Minute {
			t.Errorf("%s: window %s, want 2m", endpoint, got)
		}
	}
}

// A cluster's and an instance group's create and delete default to 3h, which
// they were measured inside, while everything else keeps operation_timeout's
// 2h. Setting operation_timeout, in the block or the environment, sets both.
func TestHardwareOperationsDefaultToTheirOwnTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		block, env string
		want, long time.Duration
	}{
		{name: "unset", want: 2 * time.Hour, long: 3 * time.Hour},
		{name: "in the block", block: "45m", want: 45 * time.Minute, long: 45 * time.Minute},
		{name: "in the environment", env: "5h", want: 5 * time.Hour, long: 5 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := saEnv(nil)
			if tc.env != "" {
				env[EnvVar(envOperationTimeout)] = tc.env
			}
			got, diags := resolveSettings(blockValues{OperationTimeout: tc.block}, envMap(env))
			if diags.HasError() {
				t.Fatalf("resolve: %v", diags)
			}
			if got.OperationTimeout != tc.want || got.LongOperationTimeout != tc.long {
				t.Errorf("operation timeout %s and long %s, want %s and %s",
					got.OperationTimeout, got.LongOperationTimeout, tc.want, tc.long)
			}
			if w := (waitConfig{Timeout: got.OperationTimeout, Long: got.LongOperationTimeout}); w.long() != tc.long {
				t.Errorf("long() = %s, want %s", w.long(), tc.long)
			}
		})
	}
}
