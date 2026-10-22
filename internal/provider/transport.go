package provider

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

// NewPlatformTransport is the connection pool one configured provider sends
// every request through: a clone of http.DefaultTransport, so the proxy, dial,
// TLS handshake and keep-alive settings are exactly Go's defaults, but a pool
// nothing else in the process holds. It's exported for the acceptance
// harness, which needs a pool of its own for the same reason.
//
// Sharing http.DefaultTransport itself would hand every other user of it the
// power to break this provider's requests. Its CloseIdleConnections races a
// request that has just picked up an idle connection, which then fails with
// "http: CloseIdleConnections called", and a DELETE or POST isn't replayed by
// net/http after that. httptest.Server.Close calls it on the default transport
// every time, which is how a unit test failed at random while a parallel
// test's server shut down. A pool of the provider's own is closed by nobody.
func NewPlatformTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}
	// Something in the process replaced the default with a RoundTripper that
	// can't be cloned. Nothing in this repository does, but the provider
	// still gets Go's defaults rather than whatever was put there.
	return goDefaultTransport()
}

// goDefaultTransport is http.DefaultTransport as net/http declares it, built
// fresh. TestTheFallbackTransportIsGoDefaults keeps it matching the clone.
func goDefaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// errNoBase is what a wrapper answers when it was built with nothing under it
// to send through. Falling back to http.DefaultTransport is the bug
// NewPlatformTransport exists to prevent, and a panic would end an apply
// with a stack trace instead of a diagnostic.
func errNoBase(wrapper string) error {
	return fmt.Errorf("the provider's %s has no transport under it to send the request through. "+
		"This is a bug in the provider", wrapper)
}

// RoundTrip passes the request on and lets annotate read a refusal. See
// region_mismatch.go.
func (t *regionMismatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	return t.annotate(req, resp), nil
}
