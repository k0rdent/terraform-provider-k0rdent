package provider_test

import (
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The naming decision is settled: this provider claims
// registry.terraform.io/k0rdent/k0rdent. The address is permanent at first
// publish, so it is pinned here.
func TestRegistryAddressIsTheSettledPublishAddress(t *testing.T) {
	t.Parallel()

	const want = "registry.terraform.io/k0rdent/k0rdent"
	if got := provider.RegistryAddress(); got != want {
		t.Fatalf("RegistryAddress() = %q, want %q; this address is permanent at first publish", got, want)
	}
}
