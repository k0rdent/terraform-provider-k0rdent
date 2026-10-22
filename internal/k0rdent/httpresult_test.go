package k0rdent

import (
	"net/http"
	"testing"
)

// The whole point of HTTPResult: a nil receiver returns nils instead of
// panicking. `XxxWithResponse` returns a nil response whenever the transport
// fails, and the caller reports the error on exactly that path.
//
// The call goes through the interface rather than the concrete pointer
// because that is how a consumer reaches it, and because a nil pointer boxed
// in an interface is a non-nil interface value: this would still panic if the
// guard were on the caller's side instead of inside the method.
func TestHTTPResult_NilReceiver(t *testing.T) {
	var r HTTPResulter = (*GetClusterResponse)(nil)

	resp, body := r.HTTPResult()
	if resp != nil {
		t.Errorf("HTTPResponse = %v, want nil", resp)
	}
	if body != nil {
		t.Errorf("Body = %q, want nil", body)
	}
}

// The stock getters are value-receiver, so the nil guard cannot live there.
// Pinned so an upgrade that makes them pointer-receiver, which would make
// HTTPResult redundant, shows up as a failure here rather than as unexplained
// duplication.
func TestStockGettersPanicOnNilPointer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("GetBody on a nil pointer did not panic; the generator may now use pointer receivers, so re-check whether HTTPResult is still needed")
		}
	}()

	var r *GetClusterResponse
	_ = r.GetBody()
}

func TestHTTPResult_ReturnsBothFields(t *testing.T) {
	want := &http.Response{StatusCode: http.StatusCreated}
	r := &GetClusterResponse{
		HTTPResponse: want,
		Body:         []byte(`{"clientSecret":"s"}`),
	}

	resp, body := r.HTTPResult()
	if resp != want {
		t.Errorf("HTTPResponse = %v, want %v", resp, want)
	}
	if string(body) != `{"clientSecret":"s"}` {
		t.Errorf("Body = %q, want the response body", body)
	}
}

// The return order is load-bearing: it lets a caller write
// CheckResponse(r.HTTPResult()) with no intermediate variables. Swapping the
// two would not fail to compile anywhere in this package, so it is pinned.
func TestHTTPResult_FeedsCheckResponse(t *testing.T) {
	r := &GetClusterResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden"},
		Body:         []byte(`{"message":"no route matched"}`),
	}

	err := CheckResponse(r.HTTPResult())
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("CheckResponse returned %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusForbidden || apiErr.Message != "no route matched" {
		t.Errorf("got %+v, want the 403 and its gateway message", apiErr)
	}
}
