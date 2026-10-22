package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Live on dev.4, a group created without `ssh_access` answered 201 and then
// settled `failed`, reason INVALID_CONFIGURATION, with the platform's fixed
// sentence for that reason as its message. The cause, a reservation with no
// SSH public keys, stayed in the platform's own logs. The diagnostic carries
// the platform's reason and message whatever the configuration, and names the
// likely cause only when the configuration left `ssh_access` out or empty, and
// only on the reason the platform reported, not on words in its message.
func TestAGroupThatFailsAfterCreateSaysWhy(t *testing.T) {
	const sentence = "the operation was rejected: the requested configuration is not valid"
	none := tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, nil)
	for name, tc := range map[string]struct {
		access  tftypes.Value
		reason  string
		message string
		advice  bool
	}{
		// `ssh_access = []` sends `"sshAccess":[]`, which the reservation
		// refuses the same way.
		"empty, invalid configuration": {tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{}),
			"INVALID_CONFIGURATION", "", true},
		"left out, another reason whose message names this one": {none, "INSUFFICIENT_CAPACITY",
			" (it names reason INVALID_CONFIGURATION)", false},
		"left out, invalid configuration": {none, "INVALID_CONFIGURATION", "", true},
		"given, invalid configuration":    {opsAccess(), "INVALID_CONFIGURATION", "", false},
		"left out, insufficient capacity": {none, "INSUFFICIENT_CAPACITY", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			body := deletingGroupBody("failed")
			body["stateReason"], body["stateMessage"] = tc.reason, sentence+tc.message
			platform := &pendingPlatform{body: body}
			r := &instanceGroupResource{meta: testClusterMeta(t, platform.serve(t).URL, time.Minute)}
			planRaw, s := testValue(t, r, with(opsGroup("active"), map[string]tftypes.Value{
				"ssh_access": tc.access, "state": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
			}))
			resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
			r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)

			errs := resp.Diagnostics.Errors()
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want one", errs)
			}
			detail := errs[0].Detail()
			if !strings.Contains(detail, "reason "+tc.reason) || !strings.Contains(detail, sentence) {
				t.Errorf("the platform's reason and message are missing:\n%s", detail)
			}
			if got := strings.Contains(detail, "`ssh_access`"); got != tc.advice {
				t.Errorf("names ssh_access = %v, want %v:\n%s", got, tc.advice, detail)
			}
		})
	}
}
