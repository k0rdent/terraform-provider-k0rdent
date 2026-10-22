package k0rdent

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func response(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     header,
	}
}

// Every status the platform documents for the operations a provider calls, in
// the contract envelope. The reasons and detail shapes are the ones named in
// the contract's own descriptions, so a change upstream shows up here.
func TestAPIError_ContractEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   int
		wantMsg    string
		wantReason string
		wantDetail []string
		wantReqID  string
	}{
		{
			name:     "400 bad request",
			status:   http.StatusBadRequest,
			body:     `{"error":{"code":400,"message":"malformed request body","requestId":"req-400"}}`,
			wantCode: 400, wantMsg: "malformed request body", wantReqID: "req-400",
		},
		{
			name:     "401 invalid credential",
			status:   http.StatusUnauthorized,
			body:     `{"error":{"code":401,"message":"invalid client credentials","requestId":"req-401"}}`,
			wantCode: 401, wantMsg: "invalid client credentials", wantReqID: "req-401",
		},
		{
			name:   "403 carries a top-level reason",
			status: http.StatusForbidden,
			body: `{"error":{"code":403,"message":"non-interactive principals may not mint credentials",` +
				`"reason":"NON_INTERACTIVE_PRINCIPAL","requestId":"req-403"}}`,
			wantCode: 403, wantMsg: "non-interactive principals may not mint credentials",
			wantReason: "NON_INTERACTIVE_PRINCIPAL", wantReqID: "req-403",
		},
		{
			name:     "404 not found is not collapsed",
			status:   http.StatusNotFound,
			body:     `{"error":{"code":404,"message":"cluster not found","requestId":"req-404"}}`,
			wantCode: 404, wantMsg: "cluster not found", wantReqID: "req-404",
		},
		{
			name:   "409 carries a per-detail reason and metadata",
			status: http.StatusConflict,
			body: `{"error":{"code":409,"message":"batch already in flight",` +
				`"details":[{"reason":"BATCH_IN_PROGRESS","message":"retry after the batch settles",` +
				`"metadata":{"batchId":"batch-7"}}],"requestId":"req-409"}}`,
			wantCode: 409, wantMsg: "batch already in flight",
			wantDetail: []string{"BATCH_IN_PROGRESS"}, wantReqID: "req-409",
		},
		{
			name:   "422 carries both a top-level and a per-detail reason",
			status: http.StatusUnprocessableEntity,
			body: `{"error":{"code":422,"message":"not enough servers","reason":"DOMAIN_NOT_OWNED",` +
				`"details":[{"reason":"INSUFFICIENT_CAPACITY","metadata":{"reservationErrorCode":"not_enough_servers"}},` +
				`{"reason":"INVALID_CONFIGURATION"}],"requestId":"req-422"}}`,
			wantCode: 422, wantMsg: "not enough servers", wantReason: "DOMAIN_NOT_OWNED",
			wantDetail: []string{"INSUFFICIENT_CAPACITY", "INVALID_CONFIGURATION"}, wantReqID: "req-422",
		},
		{
			name:     "500 internal",
			status:   http.StatusInternalServerError,
			body:     `{"error":{"code":500,"message":"internal error","requestId":"req-500"}}`,
			wantCode: 500, wantMsg: "internal error", wantReqID: "req-500",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckResponse(response(tc.status, tc.body, nil), []byte(tc.body))
			if err == nil {
				t.Fatalf("CheckResponse returned nil for status %d", tc.status)
			}
			var e *APIError
			if !errors.As(err, &e) {
				t.Fatalf("error is %T, want *APIError", err)
			}
			if e.Envelope != EnvelopeContract {
				t.Errorf("Envelope = %q, want %q", e.Envelope, EnvelopeContract)
			}
			if e.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", e.StatusCode, tc.status)
			}
			if e.Code != tc.wantCode {
				t.Errorf("Code = %d, want %d", e.Code, tc.wantCode)
			}
			if e.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", e.Message, tc.wantMsg)
			}
			if e.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", e.Reason, tc.wantReason)
			}
			if e.RequestID != tc.wantReqID {
				t.Errorf("RequestID = %q, want %q", e.RequestID, tc.wantReqID)
			}
			got := e.DetailReasons()
			if len(got) != len(tc.wantDetail) {
				t.Fatalf("DetailReasons() = %v, want %v", got, tc.wantDetail)
			}
			for i := range got {
				if got[i] != tc.wantDetail[i] {
					t.Errorf("DetailReasons()[%d] = %q, want %q", i, got[i], tc.wantDetail[i])
				}
			}
			for _, want := range append([]string{tc.wantReason}, tc.wantDetail...) {
				if want != "" && !e.HasReason(want) {
					t.Errorf("HasReason(%q) = false, want true", want)
				}
			}
		})
	}
}

// The gateway denies some requests before any service is reached and answers
// with a bare {"message": ...}, which is not the contract envelope. Decoding
// only the contract shape would leave Message empty on exactly these failures.
func TestAPIError_GatewayEnvelope(t *testing.T) {
	statuses := []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusUnprocessableEntity,
		http.StatusInternalServerError,
	}
	const body = `{"message":"You cannot consume this service"}`

	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			err := CheckResponse(response(status, body, nil), []byte(body))
			var e *APIError
			if !errors.As(err, &e) {
				t.Fatalf("error is %T, want *APIError", err)
			}
			if e.Envelope != EnvelopeGateway {
				t.Errorf("Envelope = %q, want %q", e.Envelope, EnvelopeGateway)
			}
			if e.Message != "You cannot consume this service" {
				t.Errorf("Message = %q, want the gateway message", e.Message)
			}
			if e.StatusCode != status {
				t.Errorf("StatusCode = %d, want %d", e.StatusCode, status)
			}
			// The gateway envelope carries no code; do not synthesise one
			// from the status, or a caller cannot tell what the server said.
			if e.Code != 0 {
				t.Errorf("Code = %d, want 0 for the gateway envelope", e.Code)
			}
			if !strings.Contains(e.Error(), "You cannot consume this service") {
				t.Errorf("Error() = %q, want it to carry the message", e.Error())
			}
		})
	}
}

func TestAPIError_RequestIDFromHeader(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		header http.Header
		want   string
	}{
		{
			name:   "body wins over header",
			body:   `{"error":{"code":403,"message":"denied","requestId":"from-body"}}`,
			header: http.Header{"X-Kong-Request-Id": {"from-kong"}},
			want:   "from-body",
		},
		{
			name:   "kong header when the body carries none",
			body:   `{"error":{"code":403,"message":"denied"}}`,
			header: http.Header{"X-Kong-Request-Id": {"from-kong"}},
			want:   "from-kong",
		},
		{
			name:   "service header when there is no kong header",
			body:   `{"message":"denied at the gateway"}`,
			header: http.Header{"X-Request-Id": {"from-service"}},
			want:   "from-service",
		},
		{
			name:   "kong header preferred over the service header",
			body:   ``,
			header: http.Header{"X-Request-Id": {"from-service"}, "X-Kong-Request-Id": {"from-kong"}},
			want:   "from-kong",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := NewAPIError(response(http.StatusForbidden, tc.body, tc.header), []byte(tc.body))
			if e.RequestID != tc.want {
				t.Errorf("RequestID = %q, want %q", e.RequestID, tc.want)
			}
		})
	}
}

func TestAPIError_UndecodableBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", ``},
		{"html error page", `<html><body>502 Bad Gateway</body></html>`},
		{"json without either envelope", `{"detail":"something else"}`},
		{"truncated json", `{"error":{"code":500,`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := NewAPIError(response(http.StatusInternalServerError, tc.body, nil), []byte(tc.body))
			if e.Envelope != EnvelopeNone {
				t.Errorf("Envelope = %q, want %q", e.Envelope, EnvelopeNone)
			}
			if string(e.Body) != tc.body {
				t.Errorf("Body = %q, want the raw body preserved", e.Body)
			}
			if !strings.Contains(e.Error(), "no decodable error body") {
				t.Errorf("Error() = %q, want it to say the body did not decode", e.Error())
			}
		})
	}
}

func TestCheckResponse_Success(t *testing.T) {
	for _, status := range []int{200, 201, 202, 204, 299} {
		if err := CheckResponse(response(status, ``, nil), nil); err != nil {
			t.Errorf("CheckResponse(%d) = %v, want nil", status, err)
		}
	}
	// A 3xx is not success. The client follows redirects via net/http, so a
	// 3xx reaching here is something the caller must see rather than a
	// status to swallow.
	if err := CheckResponse(response(304, ``, nil), nil); err == nil {
		t.Error("CheckResponse(304) = nil, want an error")
	}
}

func TestAPIError_DoesNotInterpretStatus(t *testing.T) {
	// D4: the client collapses no status. A 404 is an error like any other,
	// not a nil result, because in Terraform "absent" and "failed to read"
	// lead to opposite actions.
	body := `{"error":{"code":404,"message":"cluster not found"}}`
	err := CheckResponse(response(http.StatusNotFound, body, nil), []byte(body))
	if err == nil {
		t.Fatal("CheckResponse(404) = nil; a 404 must reach the caller")
	}
	var e *APIError
	if !errors.As(err, &e) || e.StatusCode != http.StatusNotFound {
		t.Fatalf("got %v, want an *APIError carrying 404", err)
	}
}

// RequestIDFromHeader is the same lookup NewAPIError falls back to, so a
// caller quoting the id of a response that isn't an error, or that it never
// decoded, names the same id the error would have.
func TestRequestIDFromHeader(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   string
	}{
		{"kong header preferred over the service header",
			http.Header{"X-Request-Id": {"from-service"}, "X-Kong-Request-Id": {"from-kong"}}, "from-kong"},
		{"service header when there is no kong header", http.Header{"X-Request-Id": {"from-service"}}, "from-service"},
		{"an empty kong header falls through", http.Header{"X-Kong-Request-Id": {""}, "X-Request-Id": {"from-service"}}, "from-service"},
		{"neither header", http.Header{"Content-Type": {"application/json"}}, ""},
		{"no header at all", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequestIDFromHeader(tc.header); got != tc.want {
				t.Errorf("RequestIDFromHeader = %q, want %q", got, tc.want)
			}
			e := NewAPIError(response(http.StatusForbidden, "", tc.header), nil)
			if e.RequestID != tc.want {
				t.Errorf("NewAPIError fell back to %q, RequestIDFromHeader says %q", e.RequestID, tc.want)
			}
		})
	}
}

// Only a service answers in the contract envelope. A gateway denial, a
// router's 404 or an HTML error page didn't reach one, and FromService is
// how a caller tells "the service said so" from "something in front of it
// did".
func TestAPIError_FromService(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"contract envelope", `{"error":{"code":404,"message":"cluster not found"}}`, true},
		{"contract envelope with only a reason", `{"error":{"reason":"PROJECT_REGION_MISMATCH"}}`, true},
		{"gateway envelope", `{"message":"no Route matched with those values"}`, false},
		{"html error page", `<html><body>404 Not Found</body></html>`, false},
		{"empty body", ``, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := NewAPIError(response(http.StatusNotFound, tc.body, nil), []byte(tc.body))
			if got := e.FromService(); got != tc.want {
				t.Errorf("FromService() = %v, want %v (envelope %q)", got, tc.want, e.Envelope)
			}
		})
	}
}

// A caller that prints the request id on its own line needs the rest of the
// error's text without a second copy of it. Everything else Error() says has
// to survive.
func TestAPIError_ErrorWithoutRequestID(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "the id was one note among others",
			body: `{"error":{"code":409,"message":"in use","reason":"CONFLICT_IN_USE",` +
				`"details":[{"reason":"BATCH_IN_PROGRESS"}],"requestId":"rid-409"}}`,
			want: "Conflict: in use (reason CONFLICT_IN_USE; detail reasons BATCH_IN_PROGRESS)",
		},
		{
			name: "the id was the only note",
			body: `{"error":{"code":409,"message":"in use","requestId":"rid-409"}}`,
			want: "Conflict: in use",
		},
		{
			name: "the id came from a header",
			body: `{"message":"denied at the gateway"}`,
			want: "Conflict: denied at the gateway",
		},
		{
			name: "an undecodable body still says so",
			body: `<html></html>`,
			want: "Conflict (no decodable error body)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{"X-Kong-Request-Id": {"rid-409"}}
			e := NewAPIError(response(http.StatusConflict, tc.body, header), []byte(tc.body))
			if got := e.ErrorWithoutRequestID(); got != tc.want {
				t.Errorf("ErrorWithoutRequestID() = %q, want %q", got, tc.want)
			}
			if !strings.Contains(e.Error(), "requestId rid-409") {
				t.Errorf("Error() = %q, want it to keep the request id", e.Error())
			}
		})
	}
}
