package provider

import (
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// loggingTransport writes one TRACE line per request: method, path, status,
// duration and the platform's correlation id. That traces a run that surprises
// without failing, which no diagnostic covers. TRACE rather than DEBUG, because
// DEBUG is where a user looks at their own configuration.
//
// Headers, bodies and query strings are never logged. The mint and kubeconfig
// bodies carry credentials, and no redaction list survives the API adding a
// field.
type loggingTransport struct{ base http.RoundTripper }

func (t loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.base == nil {
		return nil, errNoBase("request log")
	}
	ctx := req.Context()
	started := time.Now()
	tflog.Trace(ctx, "platform request", map[string]any{
		"method": req.Method,
		"path":   req.URL.Path,
	})

	resp, err := t.base.RoundTrip(req)
	elapsed := time.Since(started).Round(time.Millisecond)
	if err != nil {
		// The error is the transport's own, not the platform's: a DNS failure,
		// a refused connection, a TLS rejection. It carries no server-controlled
		// text, so logging it whole is safe and it is usually the whole answer.
		tflog.Trace(ctx, "platform request failed before a response", map[string]any{
			"method":   req.Method,
			"path":     req.URL.Path,
			"duration": elapsed.String(),
			"error":    redactDiagnosticText(err.Error()),
		})
		return nil, err
	}

	fields := map[string]any{
		"method":   req.Method,
		"path":     req.URL.Path,
		"status":   resp.StatusCode,
		"duration": elapsed.String(),
	}
	// The correlation handle the requirement actually asks for. On a failure it
	// reaches the operator through the diagnostic; on a success it has nowhere
	// else to go, and a successful request nobody can find in the gateway log
	// is the case that makes a platform-side investigation impossible.
	if id := k0rdent.RequestIDFromHeader(resp.Header); id != "" {
		fields["request_id"] = id
	}
	tflog.Trace(ctx, "platform response", fields)

	return resp, nil
}

// withRequestID adds the platform's request id to the fields of a log line
// about resp, and returns them. The error body's id comes first, since the
// contract says it mirrors the headers and the generated client reads it the
// same way. Without a response there's no id to add.
func withRequestID(fields map[string]any, resp *http.Response, body []byte) map[string]any {
	if resp == nil {
		return fields
	}
	id := k0rdent.RequestIDFromHeader(resp.Header)
	if apiErr := apiErrorOf(resp, body); apiErr != nil && apiErr.RequestID != "" {
		id = apiErr.RequestID
	}
	if id != "" {
		fields["request_id"] = id
	}
	return fields
}
