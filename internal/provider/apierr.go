package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// apiCall names one operation for error messages, e.g. "create the cluster".
//
// Every resource method funnels its failures through addAPIError so that a user
// reading a diagnostic sees the same four things every time: what the provider
// was trying to do, what the platform answered, the machine-readable reason if
// there was one, and the request id to quote in a support ticket.
type apiCall string

// addAPIErrorHiding is addAPIError that also hides a secret the call sent,
// such as a webhook's signing secret, which redactDiagnosticText doesn't know.
func addAPIErrorHiding(diags *diag.Diagnostics, secret string, what apiCall, resp *http.Response, body []byte, transportErr error) {
	var got diag.Diagnostics
	addAPIError(&got, what, resp, body, transportErr)
	for _, d := range got {
		summary, detail := d.Summary(), d.Detail()
		if secret != "" {
			summary = strings.ReplaceAll(summary, secret, "[REDACTED]")
			detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
		}
		if d.Severity() == diag.SeverityError {
			diags.AddError(summary, detail)
		} else {
			diags.AddWarning(summary, detail)
		}
	}
}

// addAPIError turns a transport error or a non-2xx response into a diagnostic.
//
// It does not interpret the status. ADR D4 keeps that out of the client, and the
// same reasoning applies one layer up: a 409 on a cluster means "a workflow is
// running", a 409 on a security-group change means something else, and a 422 is
// a rejected request body. Collapsing them into "retry" or "not found" is a
// resource-level decision, made at the call site where the lifecycle is known.
func addAPIError(diags *diag.Diagnostics, what apiCall, resp *http.Response, body []byte, transportErr error) {
	if transportErr != nil {
		if addMintError(diags, fmt.Sprintf("Could not %s", what), transportErr) {
			return
		}
		// A refused wait reaches here as a transport error with the
		// platform's answer inside it.
		diags.AddError(
			fmt.Sprintf("Could not %s", what),
			redactDiagnosticText("The request did not complete: "+withoutInlineRequestID(transportErr)+
				requestIDLine(requestIDIn(transportErr))),
		)
		return
	}

	err := k0rdent.CheckResponse(resp, body)
	if err == nil {
		// The caller believed this response was a failure and the status says
		// otherwise, which means the operation answered a status the provider
		// does not handle. Say that, rather than reporting success.
		//
		// The body is never quoted. A successful answer is where the platform
		// puts what it shows once, a webhook's signing secret for one, and a
		// diagnostic reaches the console, TF_LOG and a CI log. No redaction
		// list keeps up with the fields an answer can grow, so the status and
		// the content type are all it says.
		status, contentType, id := "no response", "", ""
		if resp != nil {
			status, contentType, id = resp.Status, resp.Header.Get("Content-Type"), k0rdent.RequestIDFromHeader(resp.Header)
		}
		if contentType == "" {
			contentType = "none given"
		}
		diags.AddError(
			fmt.Sprintf("Unexpected response when asked to %s", what),
			redactDiagnosticText(fmt.Sprintf("The platform answered %s, content type %s, which this provider "+
				"does not know how to read. The body isn't shown, because a successful answer can carry a "+
				"value the platform shows only once.", status, contentType)+requestIDLine(id)),
		)
		return
	}

	var apiErr *k0rdent.APIError
	if !errors.As(err, &apiErr) {
		diags.AddError(fmt.Sprintf("Could not %s", what), redactDiagnosticText(err.Error()))
		return
	}
	diags.AddError(fmt.Sprintf("Could not %s", what), apiErrorDetail(apiErr))
}

// addWaitError reports an error that came back through a waiter. The upstream
// detail is in there, wrapped, and it has to be unwrapped for two reasons: the
// diagnostic should carry the code and request id like any other, and the raw
// string is never redacted, so a credential echoed in a message would reach the
// console.
func addWaitError(diags *diag.Diagnostics, summary string, err error) {
	if addMintError(diags, summary, err) {
		return
	}
	var apiErr *k0rdent.APIError
	if errors.As(err, &apiErr) {
		diags.AddError(summary, apiErrorDetail(apiErr))
		return
	}
	diags.AddError(summary, redactDiagnosticText(err.Error()+requestIDLine(requestIDIn(err))))
}

// addAdvisedError is addWaitError with a closing paragraph of advice saying
// what the failure means for this operation and what to do about it, for
// refusals where the platform's message names the cause but not the way out.
// Advice may refer to that message as "above". Empty advice adds nothing.
func addAdvisedError(diags *diag.Diagnostics, summary, advice string, err error) {
	if addMintError(diags, summary, err) {
		return
	}
	detail := redactDiagnosticText(err.Error() + requestIDLine(requestIDIn(err)))
	var apiErr *k0rdent.APIError
	if errors.As(err, &apiErr) {
		detail = apiErrorDetailGuided(apiErr, advice == "")
	}
	if advice != "" {
		detail += "\n\n" + redactDiagnosticText(advice)
	}
	diags.AddError(summary, detail)
}

// addMintError reports a request that was never sent because the provider
// couldn't get an access token, and reports whether err was one. A revoked
// or mistyped credential fails every operation the same way, so the summary
// names the credential, not the resource, and the operation's own advice is
// left out because it's about an answer the operation never got.
//
// Only a 401 or 403 from the token endpoint is a refusal of the credential.
// Any other mint failure, a 503 or an unreachable endpoint, says so without
// blaming it.
func addMintError(diags *diag.Diagnostics, summary string, err error) bool {
	if !errors.Is(err, k0rdent.ErrTokenMint) {
		return false
	}
	var apiErr *k0rdent.APIError
	if !errors.As(err, &apiErr) {
		diags.AddError(summary+" because the provider couldn't get an access token",
			redactDiagnosticText(withoutInlineRequestID(err)+requestIDLine(requestIDIn(err))))
		return true
	}
	switch apiErr.StatusCode {
	case http.StatusUnauthorized:
		diags.AddError(summary+" because the platform refused the provider's credential",
			apiErrorDetail(apiErr)+"\n\n"+redactDiagnosticText(fmt.Sprintf(
				"The token endpoint doesn't accept the credential in %s and %s, or %s. Check that it's "+
					"the one issued for this endpoint, and that its service account is still enabled and "+
					"the credential hasn't been deleted. Running again won't help until it's fixed.",
				EnvVar(envClientID), EnvVar(envClientSecret), EnvVar(envAPIKey))))
	case http.StatusForbidden:
		diags.AddError(summary+" because the platform refused the provider's credential",
			apiErrorDetail(apiErr)+"\n\n"+redactDiagnosticText(
				"The token endpoint accepted the credential and won't issue it a token. "+
					"The platform's answer above says why."))
	default:
		diags.AddError(summary+" because the provider couldn't get an access token", apiErrorDetail(apiErr))
	}
	return true
}

func apiErrorDetail(apiErr *k0rdent.APIError) string {
	return apiErrorDetailGuided(apiErr, true)
}

// apiErrorDetailGuided is apiErrorDetail, with the next step from
// guidanceFor when guided is set. A caller with advice of its own for this
// operation leaves it out, so a diagnostic never gives two next steps.
func apiErrorDetailGuided(apiErr *k0rdent.APIError, guided bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The platform answered %s.", apiErr.Status)
	if apiErr.Message != "" {
		fmt.Fprintf(&b, "\n\n%s", apiErr.Message)
	}
	if apiErr.Reason != "" {
		fmt.Fprintf(&b, "\n\nReason: %s", apiErr.Reason)
	}
	if reasons := apiErr.DetailReasons(); len(reasons) > 0 {
		fmt.Fprintf(&b, "\nDetail reasons: %s", strings.Join(reasons, ", "))
	}
	writeQuotaRefusals(&b, apiErr)
	if advice := regionMismatchAdvice(apiErr); advice != "" {
		fmt.Fprintf(&b, "\n\n%s", advice)
	}
	if next := guidanceFor(apiErr); guided && next != "" {
		fmt.Fprintf(&b, "\n\n%s", next)
	}
	if apiErr.RequestID != "" {
		fmt.Fprintf(&b, "\n\nRequest id: %s", apiErr.RequestID)
	}
	switch {
	case apiErr.Envelope == k0rdent.EnvelopeGateway:
		b.WriteString("\n\nThe API gateway answered this, so the request didn't reach the service. " +
			"The gateway's answers describe an authentication, authorization or routing decision.")
	case apiErr.StatusCode == http.StatusNotFound && !serviceSaysMissing(apiErr):
		b.WriteString("\n\nThis 404 came without the service's error envelope, so a router or region " +
			"guard in front of the service answered it and the request didn't reach the service.")
	}

	return redactDiagnosticText(b.String())
}

// quotaRefusal is one details[] entry of a QUOTA_EXCEEDED answer.
type quotaRefusal struct {
	Dimension    string `json:"dimension"`
	Requested    int64  `json:"requested"`
	Admissible   int64  `json:"admissible"`
	BindingScope string `json:"bindingScope"`
	Scopes       []struct {
		Scope    string `json:"scope"`
		TargetID string `json:"targetId"`
		Limit    int64  `json:"limit"`
		Used     int64  `json:"used"`
		Headroom int64  `json:"headroom"`
	} `json:"scopes"`
}

// quotaRefusals decodes the metadata of every QUOTA_EXCEEDED detail. An entry
// whose metadata does not decode is skipped: the message above it still says
// what was refused.
func quotaRefusals(apiErr *k0rdent.APIError) []quotaRefusal {
	var out []quotaRefusal
	for _, d := range apiErr.Details {
		if d["reason"] != "QUOTA_EXCEEDED" || d["metadata"] == nil {
			continue
		}
		raw, err := json.Marshal(d["metadata"])
		if err != nil {
			continue
		}
		var q quotaRefusal
		if json.Unmarshal(raw, &q) != nil || q.Dimension == "" {
			continue
		}
		out = append(out, q)
	}
	return out
}

func writeQuotaRefusals(b *strings.Builder, apiErr *k0rdent.APIError) {
	for _, q := range quotaRefusals(apiErr) {
		fmt.Fprintf(b, "\n\nQuota %s: requested %d, admissible %d", q.Dimension, q.Requested, q.Admissible)
		if q.BindingScope != "" {
			fmt.Fprintf(b, ", binding scope %s", q.BindingScope)
		}
		b.WriteString(".")
		for _, s := range q.Scopes {
			fmt.Fprintf(b, "\n  %s %s: limit %d, used %d, headroom %d", s.Scope, s.TargetID, s.Limit, s.Used, s.Headroom)
		}
	}
}

// isNotFound reports whether a response says the resource does not exist.
//
// The client deliberately does not collapse 404s into a nil result (ADR D4), so
// every caller that cares has to ask. In Terraform the answer matters: a 404 on
// refresh means the resource is gone and state should be dropped, and a 404 on
// delete means there is nothing left to delete.
//
// Only the service that owns a resource can say it is missing, and every
// service says so with the contract's error envelope. A 404 without that
// envelope stopped short of the service: the gateway's answer for a path it
// has no route for, a service's region guard or default router for a region
// or path that version doesn't serve, or an empty no-route answer. None of
// those says anything about the resource, and taking one as "gone" would drop
// a live resource from state or end a destroy without sending the DELETE. So
// such a 404 is not "gone". It falls through to the caller's error path, where
// apiErrorDetail says what answered.
func isNotFound(resp *http.Response, body []byte) bool {
	return serviceSaysMissing(apiErrorOf(resp, body))
}

// serviceSaysMissing is the one rule for a 404 meaning the resource is gone:
// it came from the service, in the contract's error envelope. isNotFound
// applies it to a response and apiErrorDetail to a decoded error, so the two
// can't disagree about what a 404 means.
func serviceSaysMissing(apiErr *k0rdent.APIError) bool {
	return apiErr != nil && apiErr.StatusCode == http.StatusNotFound && apiErr.FromService()
}

// apiErrorOf decodes a failed response, or returns nil when it is not one.
func apiErrorOf(resp *http.Response, body []byte) *k0rdent.APIError {
	var apiErr *k0rdent.APIError
	if errors.As(k0rdent.CheckResponse(resp, body), &apiErr) {
		return apiErr
	}
	return nil
}

var bearerValue = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]+`)

// redactDiagnosticText is the final boundary before an API failure reaches
// Terraform output (including TF_LOG). APIs should not echo credentials, but
// a diagnostic must remain safe if an intermediary does. Any text the
// platform wrote goes through here on its way to a diagnostic, so this is
// also where its control characters are escaped.
//
// Both the raw and the trimmed form are replaced. resolveSettings trims every
// variable, so a secret exported with a trailing newline travels trimmed and
// would not match the untrimmed value os.Getenv returns.
func redactDiagnosticText(text string) string {
	for _, name := range []string{EnvVar(envClientID), EnvVar(envClientSecret), EnvVar(envAPIKey)} {
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		text = strings.ReplaceAll(text, value, "[REDACTED]")
		if trimmed := strings.TrimSpace(value); trimmed != "" && trimmed != value {
			text = strings.ReplaceAll(text, trimmed, "[REDACTED]")
		}
	}
	return escapeControl(bearerValue.ReplaceAllString(text, "Bearer [REDACTED]"))
}

// escapeControl shows each control character the way %q would, `\x1b` for an
// ESC, so a terminal prints it instead of acting on it. A platform message
// carrying an escape sequence could otherwise recolour the console, rewrite
// the window title or erase the lines above it. Newline and tab are how the
// provider lays a diagnostic out, and stay.
//
// It runs after redaction so that a secret is matched as it was sent, and
// before any truncation so that a cut can only shorten the escaped text, never
// leave a raw sequence behind.
//
// Invalid UTF-8 comes out two ways: text with no control character takes the
// fast path and keeps its invalid bytes, while text with one is rewritten rune
// by rune and each invalid byte becomes U+FFFD. Either is safe, since
// terraform-plugin-go replaces invalid bytes before a diagnostic is sent.
func escapeControl(text string) string {
	if strings.IndexFunc(text, isEscapedControl) < 0 {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		if isEscapedControl(r) {
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isEscapedControl is a control character other than newline and tab, or a
// line separator, a paragraph separator or a format character such as a bidi
// override. Those aren't control characters, and still let a message start a
// line or reorder what's shown.
func isEscapedControl(r rune) bool {
	return (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Cf)
}

// answerError is a response that isn't a failure and still carries nothing
// the caller can use. It isn't an *k0rdent.APIError, because the platform
// didn't fail and a caller that branches on one mustn't see it, but it keeps
// the request id so the diagnostic can still name the answer.
type answerError struct {
	status, requestID, says string
}

func (e *answerError) Error() string { return "the platform answered " + e.status + e.says }

func newAnswerError(resp *http.Response, says string) error {
	if resp == nil {
		return &answerError{status: "no response", says: says}
	}
	return &answerError{status: resp.Status, requestID: k0rdent.RequestIDFromHeader(resp.Header), says: says}
}

// answeredWithout is the error for a success with no body, e.g. "the platform
// answered 200 OK with no cluster".
func answeredWithout(resp *http.Response, what string) error {
	return newAnswerError(resp, " with no "+what)
}

// requestIDIn finds the request id of whatever answer err carries, wrapped or
// not, so a diagnostic built from an error rather than a response can still
// name it.
func requestIDIn(err error) string {
	var apiErr *k0rdent.APIError
	if errors.As(err, &apiErr) {
		return apiErr.RequestID
	}
	var answer *answerError
	if errors.As(err, &answer) {
		return answer.requestID
	}
	return ""
}

// withoutInlineRequestID is err's text with the request id left out of the
// platform's answer inside it, which APIError.Error writes as a note. The
// diagnostic gives the id its own line, as every other diagnostic does, and
// one copy is what a support ticket needs.
func withoutInlineRequestID(err error) string {
	var apiErr *k0rdent.APIError
	if !errors.As(err, &apiErr) {
		return err.Error()
	}
	return strings.Replace(err.Error(), apiErr.Error(), apiErr.ErrorWithoutRequestID(), 1)
}

// requestIDLine is the closing line apiErrorDetail writes, for details built
// some other way.
func requestIDLine(id string) string {
	if id == "" {
		return ""
	}
	return "\n\nRequest id: " + id
}
