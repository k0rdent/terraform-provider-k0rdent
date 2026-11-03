package provider

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The plan's warning on a cluster read `failed` ends with the advice to set
// `on_failed = "keep"` only where the plan waits for the cluster. A cluster
// already kept plans nothing, and so does one a cancel left `failed`, kept or
// not, so the advice would ask for what's already the case.
func TestAFailedClusterWarningAdvisesKeepOnlyWhereThePlanWaits(t *testing.T) {
	const advice = "To keep the cluster as it is, and plan no change for it, set `on_failed = \"keep\"`."
	for _, tc := range []struct {
		name   string
		reason string
		kept   bool
		advise bool
	}{
		{"waited on", "RESERVATION_FAILED", false, true},
		{"waited on, no reason recorded", "", false, true},
		{"kept", "RESERVATION_FAILED", true, false},
		{"after a cancel", string(k0rdent.StateReasonCANCELLED), false, false},
		{"after a cancel, kept", string(k0rdent.StateReasonCANCELLED), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnFailed(&diags, "c1", failedRead{Reason: tc.reason}, tc.kept, time.Minute)
			warnings := diags.Warnings()
			if len(warnings) != 1 || diags.HasError() {
				t.Fatalf("want one warning and no error, got %v", diags)
			}
			detail := warnings[0].Detail()
			if got := strings.Contains(detail, advice); got != tc.advise {
				t.Errorf("advises keep = %t, want %t:\n%s", got, tc.advise, detail)
			}
			if tc.advise && !strings.HasSuffix(detail, advice) {
				t.Errorf("the advice isn't the warning's last sentence:\n%s", detail)
			}
		})
	}
}
