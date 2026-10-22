package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// A pool URI's segments arrive unescaped, so one that decodes to a dot
// segment or a slash is refused here, as a bare id holding one would be,
// rather than left to the client.
func TestAddressPoolRefHoldsEachSegmentToOne(t *testing.T) {
	region := types.StringValue("test")
	for ref, want := range map[string]bool{
		"/v1/regions/us-oregon-1/networking/address-pools/pool-a": true,
		"pool-a": true,
		"/v1/regions/us-oregon-1/networking/address-pools/%2E%2E": false,
		"/v1/regions/us-oregon-1/networking/address-pools/a%2Fb":  false,
		"/v1/regions/%2E/networking/address-pools/pool-a":         false,
	} {
		gotRegion, gotID, ok := addressPoolRef(ref, region)
		if ok != want {
			t.Errorf("addressPoolRef(%q) = %q, %q, %v, want ok %v", ref, gotRegion, gotID, ok, want)
		}
	}
}
