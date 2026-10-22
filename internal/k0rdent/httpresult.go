package k0rdent

import "net/http"

// HTTPResulter is implemented by every generated `*XxxResponse`. It exists so
// a caller can report a failure from any operation through one function
// instead of one per response type.
//
// The generated response types have no common interface: each is a distinct
// struct, which is why [CheckResponse] takes the response and body rather
// than the response value. Pulling those two fields out at the call site is
// five lines, and a consumer that reports errors uniformly writes them once
// per operation. In the Terraform provider that is 42 copies of:
//
//	var body []byte
//	var httpResp *http.Response
//	if created != nil {
//	        body, httpResp = created.Body, created.HTTPResponse
//	}
//
// HTTPResult collapses that to one call.
//
// # Why the stock getters cannot do this
//
// oapi-codegen generates GetBody, Status, StatusCode and ContentType with
// value receivers, so an interface holding them is satisfied by the value and
// not usefully by the pointer: calling one on a nil `*XxxResponse`
// dereferences and panics. The nil case is not hypothetical, it is the normal
// one. When the transport fails, `XxxWithResponse` returns a nil response and
// a non-nil error, which is exactly when the caller wants to report.
//
// Nor can the caller guard it. A nil `*XxxResponse` stored in an interface is
// a non-nil interface value, so `if r != nil` inside a helper is true and the
// panic happens anyway. The guard has to be inside a method with a pointer
// receiver, which is what HTTPResult is.
//
// # Why one method returning a pair
//
// Describing a failure needs both the response and the undecoded body: the
// body because the request id and the reason live there, the response because
// a gateway denial may send neither. Two accessors would mean two nil guards
// and two calls at every site, which is most of the prelude back again. The
// return order matches [CheckResponse], so they compose:
//
//	if err := CheckResponse(r.HTTPResult()); err != nil {
//	        return err
//	}
//
// # Why the name
//
// Not GetHTTPResponse: that is the name a future oapi-codegen release would
// most plausibly pick for a stock value-receiver getter, and Go does not let
// one method name mix receiver kinds. A distinct name keeps the generator
// free to add its own without a conflict that would only surface on upgrade.
type HTTPResulter interface {
	// HTTPResult returns the raw response and the undecoded body. Both are
	// nil when the receiver is nil, and HTTPResponse can be nil on its own
	// when a response was parsed from nothing.
	HTTPResult() (*http.Response, []byte)
}

// One generated type, asserted at compile time. The method comes from a
// vendored template override, so this is the thing that fails loudly if a
// generator upgrade drops the override rather than leaving the provider to
// discover it.
var _ HTTPResulter = (*MintTokenResponse)(nil)
