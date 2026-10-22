package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// hostile carries what a terminal acts on rather than prints: a bare ESC, an
// OSC title rewrite ended by BEL, a CSI colour, DEL and the one-byte C1 CSI.
// encoding/json decodes `\u001b` in a body to the raw byte, so a platform
// answer can put any of these in a field the provider quotes.
const hostile = "a\x1bb \x1b]0;x\x07 \x1b[31mred \x7f \u009b2J\rz"

// assertNoControlRunes fails for any control character a diagnostic would
// hand to a terminal. Newline and tab are layout, and stay.
func assertNoControlRunes(t *testing.T, diags diag.Diagnostics) {
	t.Helper()
	if len(diags) == 0 {
		t.Fatal("no diagnostic was added")
	}
	for _, d := range diags {
		for field, text := range map[string]string{"summary": d.Summary(), "detail": d.Detail()} {
			for _, r := range text {
				if unicode.IsControl(r) && r != '\n' && r != '\t' {
					t.Errorf("%s carries control character %U:\n%q", field, r, text)
					break
				}
			}
		}
		// The text is shown escaped, not dropped, so it can still be quoted
		// in a support ticket.
		if !strings.Contains(d.Detail(), `\x1b`) {
			t.Errorf("detail does not show the escape visibly:\n%q", d.Detail())
		}
	}
}

func hostileResponse(t *testing.T, status int, body any) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, hostile),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
	return resp, raw
}

func TestPlatformTextInAnAPIErrorIsEscaped(t *testing.T) {
	t.Parallel()
	resp, body := hostileResponse(t, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{
		"code": 422, "message": hostile, "reason": hostile, "requestId": hostile,
		"details": []any{map[string]any{
			"reason": "QUOTA_EXCEEDED",
			"metadata": map[string]any{
				"dimension": hostile, "requested": 2, "admissible": 1, "bindingScope": hostile,
				"scopes": []any{map[string]any{"scope": hostile, "targetId": hostile, "limit": 1, "used": 1}},
			},
		}},
	}})
	var diags diag.Diagnostics
	addAPIError(&diags, "create the cluster", resp, body, nil)
	assertNoControlRunes(t, diags)
}

// A 2xx the caller did not expect is quoted by its status line and content
// type, and the body never.
func TestAnUnreadableAnswerIsEscaped(t *testing.T) {
	t.Parallel()
	resp := &http.Response{StatusCode: http.StatusOK, Status: "200 " + hostile,
		Header: http.Header{"Content-Type": []string{hostile}}}
	var diags diag.Diagnostics
	addAPIError(&diags, "create the cluster", resp, []byte(hostile), nil)
	assertNoControlRunes(t, diags)
}

func TestATransportErrorIsEscaped(t *testing.T) {
	t.Parallel()
	var diags diag.Diagnostics
	addAPIError(&diags, "create the cluster", nil, nil, errors.New(hostile))
	assertNoControlRunes(t, diags)
}

// A cluster's `stateMessage` reaches the console through a wait's error.
func TestAClusterStateMessageIsEscaped(t *testing.T) {
	t.Parallel()
	message := hostile
	failed := k0rdent.ClusterStateFailed
	err := fmt.Errorf("the cluster is failed%s", clusterStates.detail(k0rdent.Cluster{State: &failed, StateMessage: &message}))
	var diags diag.Diagnostics
	addWaitError(&diags, "Could not create the cluster", err)
	assertNoControlRunes(t, diags)
}

func TestAKubeconfigFailureReasonIsEscaped(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "active", issueFailed: true,
		failureReason: hostile, window: 50 * time.Millisecond}
	_, diags := openTestKubeconfig(t, p, time.Hour)
	assertNoControlRunes(t, diags)
}

func TestAFailedClusterStateMessageOnAKubeconfigIsEscaped(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "failed", stateMessage: hostile,
		window: 50 * time.Millisecond}
	_, diags := openTestKubeconfig(t, p, time.Hour)
	assertNoControlRunes(t, diags)
}

// Escaping runs after redaction, so a secret is still found and replaced, and
// a credential that itself carries a control character is not split by it.
func TestEscapingKeepsRedaction(t *testing.T) {
	t.Setenv(EnvVar(envClientSecret), "s3cr\x1bt")
	got := redactDiagnosticText("echoed s3cr\x1bt and Bearer abc.def\x07")
	if strings.Contains(got, "s3cr") || strings.Contains(got, "abc.def") {
		t.Errorf("a credential survived: %q", got)
	}
	want := `echoed [REDACTED] and Bearer [REDACTED]\a`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A destroy warning quotes the addresses a grant resolved to and the store's
// ID, both written by the platform.
func TestADestroyWarningIsEscaped(t *testing.T) {
	t.Parallel()
	report := grantReport{"ig-a": {
		state:     string(k0rdent.FileStoreConsumerStatusStateActive),
		addresses: []string{"10.0.0.1", hostile},
	}}
	assertNoControlRunes(t, report.destroyWarning("fs-"+hostile))
}

// A line or paragraph separator starts a new line in some terminals and
// editors, and a bidi override reorders what follows it, so a message could
// pass for another line or show text other than it holds. Neither is a
// control character, so each is escaped by its class: Zl, Zp and Cf.
func TestSeparatorsAndFormatCharactersAreEscaped(t *testing.T) {
	const text = "a\u2028b\u2029c\u202edcba\u2066d\u200be\nf\tg"
	for name, got := range map[string]string{"escapeControl": escapeControl(text), "oneLine": oneLine(text)} {
		for _, r := range got {
			if unicode.In(r, unicode.Zl, unicode.Zp, unicode.Cf) {
				t.Errorf("%s left %U in %q", name, r, got)
			}
		}
		for _, want := range []string{"\\u2028", "\\u2029", "\\u202e", "\\u2066", "\\u200b"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s doesn't show %s: %q", name, want, got)
			}
		}
	}
	if got := escapeControl(text); !strings.Contains(got, "e\nf\tg") {
		t.Errorf("escapeControl lost its layout: %q", got)
	}
	if got := oneLine(text); strings.ContainsAny(got, "\n\t") {
		t.Errorf("oneLine kept a newline or tab: %q", got)
	}
}

// A 2xx the caller couldn't read is reported by its status and content type,
// never its body. A successful answer is where a platform puts what it shows
// once, a signing secret for one, and a diagnostic reaches the console,
// TF_LOG and a CI log.
func TestASuccessfulAnswerIsNeverQuoted(t *testing.T) {
	t.Parallel()
	resp := &http.Response{StatusCode: http.StatusOK, Status: "200 OK",
		Header: http.Header{"Content-Type": []string{"text/plain"}}}
	var diags diag.Diagnostics
	addAPIError(&diags, "create the webhook subscription", resp,
		[]byte(`{"id":"x","signing_secret":"whsec_leaked_value"}`), nil)
	if !diags.HasError() {
		t.Fatal("an unreadable 2xx reported no error")
	}
	detail := diags.Errors()[0].Detail()
	if strings.Contains(detail, "whsec_leaked_value") || strings.Contains(detail, "signing_secret") {
		t.Errorf("the diagnostic quotes the body: %q", detail)
	}
	if !strings.Contains(detail, "200") || !strings.Contains(detail, "text/plain") {
		t.Errorf("the diagnostic doesn't name the status and content type: %q", detail)
	}
}
