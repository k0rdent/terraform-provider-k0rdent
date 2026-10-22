package k0rdent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Request-id response headers, in the order they are trusted. The contract says
// `error.requestId` mirrors the gateway's `X-Kong-Request-Id` and the services'
// `X-Request-Id`, so when the body carries no id these are the same value.
var requestIDHeaders = []string{"X-Kong-Request-Id", "X-Request-Id"}

// Envelope records which error body shape the server actually sent.
//
// There are two, and they disagree. The contract declares
// `{error: {code, message, reason, requestId, details}}`, but denials at the
// gateway -- before any service is reached -- return a bare
// `{"message": "..."}` instead. A client that decodes only the contract shape
// reads an empty reason on exactly the failures where the reason matters most,
// which is every 403 the gateway raises.
//
// Keeping the shape on the error makes that visible rather than silent: a
// caller can tell "the server said nothing about a reason" from "the server
// could not have said anything about a reason".
type Envelope string

const (
	// EnvelopeContract is the documented `{error: {...}}` shape.
	EnvelopeContract Envelope = "contract"
	// EnvelopeGateway is the gateway's undocumented bare `{"message": "..."}`.
	EnvelopeGateway Envelope = "gateway"
	// EnvelopeNone means the body was empty, not JSON, or neither known shape.
	EnvelopeNone Envelope = "none"
)

// ErrTokenMint is wrapped by every failure to get an access token: a refused
// or unreachable mint, a grant that could not be built, or a mint answer with
// no token in it. A 401 on an operation and a 401 on the mint read the same,
// and only the second says the credential itself is the problem, so a caller
// checks [errors.Is] rather than the text. The mint's own *APIError, when
// there is one, is still in the chain.
//
// One failure is not wrapped. A call whose context ends while it waits for
// another call's mint gets ctx.Err() as it is, because nothing refused it and
// the credential may be fine. A context that ends during the call's own mint
// is wrapped, since that mint failed.
//
// It is a sentinel value, not a second error type, so D3 still holds.
var ErrTokenMint = errors.New("mint access token")

// APIError is the only error type the hand-written layer defines (ADR D3).
// The other, [PathParamError], is generated with the request builders and is
// returned before anything is sent. APIError carries
// everything the platform says about a failure and interprets none of it: no
// status is collapsed, no 404 is turned into a nil result, no retry is implied.
// Deciding what a 409 means is the caller's job, because in Terraform that
// decision is the difference between a no-op and a destroy.
//
// Details stays in its generated shape. The contract declares
// `error.details[]` as a bare `type: object` with no properties, so the
// generated Go is `[]map[string]interface{}` and there is no typed detail to
// decode into. Writing one here would be a hand-written response struct, which
// D1 forbids -- it is filed as a spec finding instead. [APIError.DetailReasons]
// and [APIError.HasReason] read the conventional keys out of the maps.
type APIError struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Status is the HTTP status line, e.g. "403 Forbidden".
	Status string

	// Envelope is which body shape decoded.
	Envelope Envelope

	// Code is `error.code`. The contract describes it as the HTTP status
	// repeated in the body; it is reported as sent, not reconciled with
	// StatusCode.
	Code int
	// Message is `error.message`, or the gateway envelope's `message`.
	Message string
	// Reason is the top-level `error.reason`: a machine-readable failure
	// class, present when the status alone does not distinguish one.
	Reason string
	// Details is `error.details[]`, verbatim.
	Details []map[string]any

	// RequestID is `error.requestId`, falling back to the request-id response
	// header when the body carries none.
	RequestID string

	// Header is the full response header set.
	Header http.Header
	// Body is the undecoded response body, so a caller can see what a
	// third envelope shape said.
	Body []byte
}

// CheckResponse returns an *APIError for any non-2xx response and nil
// otherwise. Pass the HTTPResponse and Body fields the generated client
// already returns:
//
//	r, err := c.ListClustersWithResponse(ctx, region, project, nil)
//	if err != nil {
//	        return err
//	}
//	if err := k0rdent.CheckResponse(r.HTTPResponse, r.Body); err != nil {
//	        return err
//	}
//
// It takes the response and body rather than the generated response value
// because every generated response type is a distinct struct with no common
// interface, and reflection here would trade a compile-time signature for a
// runtime surprise.
func CheckResponse(resp *http.Response, body []byte) error {
	if resp == nil {
		return nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return NewAPIError(resp, body)
}

// NewAPIError builds an *APIError from a response and its body, whatever the
// status. It never returns nil.
func NewAPIError(resp *http.Response, body []byte) *APIError {
	e := &APIError{Body: body}
	if resp != nil {
		e.StatusCode = resp.StatusCode
		e.Status = resp.Status
		e.Header = resp.Header
	}
	e.Envelope = e.decode(body)
	if e.RequestID == "" {
		e.RequestID = RequestIDFromHeader(e.Header)
	}
	return e
}

// decode fills the body-sourced fields and reports which shape it found.
func (e *APIError) decode(body []byte) Envelope {
	if len(body) == 0 {
		return EnvelopeNone
	}

	// The contract shape, via the generated envelope type.
	var contract Error
	if err := json.Unmarshal(body, &contract); err == nil {
		inner := contract.Error
		// encoding/json does not enforce `required`, so a gateway body
		// unmarshals into this struct as all-zero. Treat the envelope as
		// present only if it carried something.
		if inner.Code != 0 || inner.Message != "" || inner.Reason != nil ||
			inner.RequestId != nil || inner.Details != nil {
			e.Code = inner.Code
			e.Message = inner.Message
			if inner.Reason != nil {
				e.Reason = *inner.Reason
			}
			if inner.RequestId != nil {
				e.RequestID = *inner.RequestId
			}
			if inner.Details != nil {
				e.Details = *inner.Details
			}
			return EnvelopeContract
		}
	}

	// The gateway shape. Decoded as a map rather than a struct: it is not in
	// the contract, so there is no generated type for it and D1 does not
	// allow inventing one.
	var loose map[string]any
	if err := json.Unmarshal(body, &loose); err == nil {
		if msg, ok := loose["message"].(string); ok && msg != "" {
			e.Message = msg
			if id, ok := loose["requestId"].(string); ok {
				e.RequestID = id
			}
			return EnvelopeGateway
		}
	}

	return EnvelopeNone
}

// RequestIDFromHeader returns the platform's request id from a response's
// headers, `X-Kong-Request-Id` first and then `X-Request-Id`, or "" when
// neither is set. It is the lookup [NewAPIError] falls back to, exported so a
// caller quoting the id of a successful response names the same one.
func RequestIDFromHeader(h http.Header) string {
	for _, name := range requestIDHeaders {
		if v := h.Get(name); v != "" {
			return v
		}
	}
	return ""
}

// DetailReasons returns the `reason` of each entry in `error.details[]`, in
// order, skipping entries that carry none.
//
// The contract puts the machine-readable failure class in one of two places
// depending on the operation: top level as `error.reason` where the status
// alone is ambiguous, or per detail where structured recovery data travels
// with it. Branching on the class means checking both, which is what
// [APIError.HasReason] does.
func (e *APIError) DetailReasons() []string {
	var out []string
	for _, d := range e.Details {
		if r, ok := d["reason"].(string); ok && r != "" {
			out = append(out, r)
		}
	}
	return out
}

// FromService reports whether the body was the contract's error envelope,
// which only a service sends. A gateway denial, a router's 404 or an
// undecodable body came from something in front of the service, so what it
// says is not the service's answer about the resource.
func (e *APIError) FromService() bool {
	return e.Envelope == EnvelopeContract
}

// HasReason reports whether reason appears as the top-level `error.reason` or
// as any `error.details[].reason`.
func (e *APIError) HasReason(reason string) bool {
	if e.Reason == reason {
		return true
	}
	for _, r := range e.DetailReasons() {
		if r == reason {
			return true
		}
	}
	return false
}

func (e *APIError) Error() string {
	return e.format(true)
}

// ErrorWithoutRequestID is [APIError.Error] with the request id left out, for
// a caller that prints [APIError.RequestID] on its own line and wants it
// there once.
func (e *APIError) ErrorWithoutRequestID() string {
	return e.format(false)
}

func (e *APIError) format(withRequestID bool) string {
	var b strings.Builder
	if e.Status != "" {
		b.WriteString(e.Status)
	} else {
		fmt.Fprintf(&b, "%d", e.StatusCode)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}

	var notes []string
	if e.Reason != "" {
		notes = append(notes, "reason "+e.Reason)
	}
	if rs := e.DetailReasons(); len(rs) > 0 {
		notes = append(notes, "detail reasons "+strings.Join(rs, ","))
	}
	if withRequestID && e.RequestID != "" {
		notes = append(notes, "requestId "+e.RequestID)
	}
	if e.Envelope == EnvelopeNone && e.Message == "" {
		notes = append(notes, "no decodable error body")
	}
	if len(notes) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(notes, "; "))
		b.WriteString(")")
	}
	return b.String()
}
