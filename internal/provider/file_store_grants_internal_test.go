package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type memPrivate map[string][]byte

func (m memPrivate) GetKey(_ context.Context, key string) ([]byte, diag.Diagnostics) {
	return m[key], nil
}

func (m memPrivate) SetKey(_ context.Context, key string, value []byte) diag.Diagnostics {
	if value == nil {
		delete(m, key)
		return nil
	}
	m[key] = value
	return nil
}

func grant(state, reason string, addrs ...string) grantStatus {
	return grantStatus{state: state, reason: reason, addresses: addrs}
}

func drifted(addrs ...string) grantStatus {
	return grant("failed", "GROUP_MEMBERSHIP_DRIFTED", addrs...)
}

func ungrantable(addrs ...string) grantStatus {
	return grant("failed", "GROUP_NO_LONGER_GRANTABLE", addrs...)
}

func sameRefs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// The adapter is the one place `consumer_status` is decoded, so the addresses
// it hands on are sorted whatever order the set iterates in.
func TestGrantReportDecodesConsumerStatus(t *testing.T) {
	ctx := context.Background()
	addrs, d := types.SetValueFrom(ctx, types.StringType, []string{"10.0.1.8", "10.0.1.7"})
	if d.HasError() {
		t.Fatal(d)
	}
	statuses, d := types.MapValueFrom(ctx, fileStoreConsumerStatusType(), map[string]fileStoreConsumerStatusModel{
		"trainers": {
			State: types.StringValue("failed"), StateReason: types.StringValue("GROUP_MEMBERSHIP_DRIFTED"),
			ResolvedAddresses: addrs,
		},
		"10.0.2.3": {
			State: types.StringValue("active"), StateReason: types.StringNull(),
			ResolvedAddresses: types.SetNull(types.StringType),
		},
	})
	if d.HasError() {
		t.Fatal(d)
	}
	var diags diag.Diagnostics
	got := grantReportOf(ctx, statuses, &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	want := grantReport{
		"trainers": drifted("10.0.1.7", "10.0.1.8"),
		"10.0.2.3": grant("active", ""),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded %#v, want %#v", got, want)
	}
	if got := grantReportOf(ctx, types.MapUnknown(fileStoreConsumerStatusType()), &diags); len(got) != 0 {
		t.Errorf("an unknown map decoded to %v", got)
	}
}

// Only a configured entry gets a verdict. An entry already removed from
// configuration is going anyway, and its verdict asks nothing of the plan.
func TestJudgeGrantsReadsOnlyConfiguredEntries(t *testing.T) {
	report := grantReport{
		"trainers":   drifted("10.0.1.7"),
		"inference":  drifted("10.0.1.9"),
		"retired":    ungrantable("10.0.1.3"),
		"evaluators": ungrantable("10.0.1.5"),
		"10.0.2.3":   grant("active", ""),
		"empty":      grant("failed", "GROUP_MEMBER_SET_EMPTY"),
	}
	v := judgeGrants(report, []string{"trainers", "inference", "evaluators", "10.0.2.3", "empty"}, nil)
	sameRefs(t, "resend", v.resend, "inference", "trainers")
	sameRefs(t, "held", v.held)
	sameRefs(t, "ungrantable", v.ungrantable, "evaluators")
}

// A refused re-send is held back while the group's addresses stay where they
// were when it was refused, and proposed again once they move.
func TestJudgeGrantsHoldsARefusedResendUntilTheReportMoves(t *testing.T) {
	refused := refusedResends{}
	refused.remember(grantReport{"trainers": drifted("10.0.1.8", "10.0.1.7")}, []string{"trainers"})

	v := judgeGrants(grantReport{"trainers": drifted("10.0.1.7", "10.0.1.8")}, []string{"trainers"}, refused)
	sameRefs(t, "resend", v.resend)
	sameRefs(t, "held", v.held, "trainers")

	v = judgeGrants(grantReport{"trainers": drifted("10.0.1.7", "10.0.1.9")}, []string{"trainers"}, refused)
	sameRefs(t, "resend after the addresses moved", v.resend, "trainers")
	sameRefs(t, "held after the addresses moved", v.held)
}

// A re-send carries every entry, so a second group's refusal must not erase
// the first's. Overwriting made two refused groups take turns being proposed,
// and the plan never went clean.
func TestRefusedDriftAccumulatesAcrossGroups(t *testing.T) {
	ctx := context.Background()
	report := grantReport{"a": drifted(), "b": drifted()}
	configured := []string{"a", "b"}
	private := memPrivate{}
	var diags diag.Diagnostics

	for _, ref := range configured {
		refused := readRefused(ctx, private, &diags)
		refused.remember(report, []string{ref})
		diags.Append(writeRefused(ctx, private, refused)...)
	}
	if v := judgeGrants(report, configured, readRefused(ctx, private, &diags)); len(v.resend) != 0 {
		t.Errorf("still proposing %v after both were refused", v.resend)
	}

	refused := readRefused(ctx, private, &diags)
	refused.forget([]string{"b"})
	diags.Append(writeRefused(ctx, private, refused)...)
	v := judgeGrants(report, configured, readRefused(ctx, private, &diags))
	sameRefs(t, "after b went through, resend", v.resend, "b")

	refused = readRefused(ctx, private, &diags)
	refused.forget([]string{"a"})
	diags.Append(writeRefused(ctx, private, refused)...)
	if _, kept := private[refusedDriftKey]; kept {
		t.Error("an empty memory was written instead of cleared")
	}
	if diags.HasError() {
		t.Fatal(diags)
	}
}

// The plan says what each verdict means for the entry it names, and an
// ungrantable group stops the plan before a re-send is proposed.
func TestPlanDiagnosticsNameEachEntry(t *testing.T) {
	diags := grantVerdicts{resend: []string{"trainers"}, held: []string{"inference"}}.planDiagnostics()
	if diags.HasError() || diags.WarningsCount() != 2 {
		t.Fatalf("want two warnings: %v", diags)
	}
	for i, want := range []struct{ ref, summary string }{
		{"inference", "A consumer group cannot be resolved again yet"},
		{"trainers", "A consumer group will be resolved again"},
	} {
		d, ok := diags[i].(diag.DiagnosticWithPath)
		if !ok || !d.Path().Equal(path.Root("consumers").AtMapKey(want.ref)) || d.Summary() != want.summary {
			t.Errorf("diagnostic %d is %v, want %q on %s", i, diags[i], want.summary, want.ref)
		}
	}

	diags = grantVerdicts{resend: []string{"trainers"}, ungrantable: []string{"retired"}}.planDiagnostics()
	if diags.ErrorsCount() != 1 || diags.WarningsCount() != 0 {
		t.Fatalf("want only the refusal: %v", diags)
	}
	if !strings.Contains(diags.Errors()[0].Detail(), `"retired"`) {
		t.Errorf("the refusal does not name the group: %q", diags.Errors()[0].Detail())
	}
}

// The plan and the update refuse an ungrantable group for the same reason, so
// they say the same thing, about every group, whichever of them catches it.
func TestAnUngrantableGroupIsRefusedOneWay(t *testing.T) {
	const summary = "A consumer group can no longer be granted"
	detail := func(ref string) string {
		return `The platform reports that the instance group "` + ref + `" is being deleted, or that ` +
			`its id now names a different group than the one the grant came from. Sending the entry ` +
			`again would grant whatever machines the id names now, so nothing is sent. Remove the ` +
			`entry, or point it at the group that should hold the access.`
	}
	v := grantVerdicts{ungrantable: []string{"evaluators", "retired"}}
	for name, diags := range map[string]diag.Diagnostics{
		"plan": v.planDiagnostics(), "apply": v.ungrantableErrors(),
	} {
		if diags.ErrorsCount() != 2 || len(diags) != 2 {
			t.Fatalf("%s: %d diagnostics, want one error per group: %v", name, len(diags), diags)
		}
		for i, ref := range v.ungrantable {
			d, ok := diags[i].(diag.DiagnosticWithPath)
			if !ok || !d.Path().Equal(path.Root("consumers").AtMapKey(ref)) {
				t.Errorf("%s: error %d is not on %s: %v", name, i, ref, diags[i])
				continue
			}
			if d.Summary() != summary {
				t.Errorf("%s: summary %q, want %q", name, d.Summary(), summary)
			}
			if d.Detail() != detail(ref) {
				t.Errorf("%s: detail\n%q\nwant\n%q", name, d.Detail(), detail(ref))
			}
		}
	}
}

// A drifted grant is still enforced, so it reads as a membership change. Any
// other failed grant is one this consumer cannot mount.
func TestFailureWarningsTellDriftFromAFailedGrant(t *testing.T) {
	diags := grantReport{
		"trainers": drifted("10.0.1.7"),
		"10.0.2.3": grant("failed", "STORAGE_ERROR"),
		"10.0.2.4": grant("active", "", "10.0.2.4"),
	}.failureWarnings()
	if diags.WarningsCount() != 2 {
		t.Fatalf("%d warnings, want 2: %v", diags.WarningsCount(), diags)
	}
	if w := diags.Warnings()[0]; !strings.Contains(w.Summary(), "did not settle") || !strings.Contains(w.Detail(), "10.0.2.3") {
		t.Errorf("first warning %q / %q", w.Summary(), w.Detail())
	}
	if w := diags.Warnings()[1]; !strings.Contains(w.Summary(), "changed membership") || !strings.Contains(w.Detail(), "trainers") {
		t.Errorf("second warning %q / %q", w.Summary(), w.Detail())
	}
}

func destroyWarnings(report grantReport) diag.Diagnostics {
	return report.destroyWarning("training-datasets")
}

func TestDestroyWarnsNamingEveryEnforcedConsumer(t *testing.T) {
	diags := destroyWarnings(grantReport{
		"inference-fleet": grant("active", "", "10.0.1.7", "10.0.1.8"),
		"10.0.2.3":        grant("active", ""),
	})

	if diags.WarningsCount() != 1 {
		t.Fatalf("%d warnings, want 1: %v", diags.WarningsCount(), diags)
	}
	w := diags.Warnings()[0]
	if !strings.Contains(w.Summary(), "training-datasets") || !strings.Contains(w.Summary(), "2 consumer") {
		t.Errorf("summary does not name the store and the count: %q", w.Summary())
	}
	for _, want := range []string{"inference-fleet", "10.0.1.7", "10.0.2.3"} {
		if !strings.Contains(w.Detail(), want) {
			t.Errorf("detail does not name %q: %q", want, w.Detail())
		}
	}
}

// A drift verdict on an established grant writes only the state and the
// reason; the addresses it authored stay enforced. So destroying the store
// still cuts those machines off.
func TestDestroyCountsAGrantADriftVerdictLeftInPlace(t *testing.T) {
	for _, reason := range []string{
		"GROUP_MEMBERSHIP_DRIFTED", "GROUP_NO_LONGER_GRANTABLE", "GROUP_MEMBER_SET_EMPTY",
		"GROUP_NO_READY_HOSTS", "NETWORK_MATCHED_NO_INTERFACE",
	} {
		diags := destroyWarnings(grantReport{
			"inference-fleet": grant("failed", reason, "10.0.1.7"),
		})
		if diags.WarningsCount() != 1 {
			t.Errorf("%s: %d warnings, want 1", reason, diags.WarningsCount())
			continue
		}
		if w := diags.Warnings()[0]; !strings.Contains(w.Summary(), "will cut off 1") ||
			!strings.Contains(w.Detail(), "10.0.1.7") {
			t.Errorf("%s: not reported as held: %q / %q", reason, w.Summary(), w.Detail())
		}
	}
}

// The same reasons at write time, before anything was authored, leave no
// addresses behind and nothing enforced.
func TestDestroyIgnoresAVerdictThatAuthoredNothing(t *testing.T) {
	diags := destroyWarnings(grantReport{
		"empty-group":  grant("failed", "GROUP_MEMBER_SET_EMPTY"),
		"not-ready":    grant("failed", "GROUP_NO_READY_HOSTS"),
		"no-interface": grant("failed", "NETWORK_MATCHED_NO_INTERFACE"),
	})
	if diags.WarningsCount() != 0 {
		t.Errorf("warned about grants that never reached a host: %v", diags)
	}
}

// Access being revoked, or that failed outright, is not access anybody holds.
// GRANT_ENFORCEMENT_UNAVAILABLE says outright that nothing is enforced.
func TestDestroyIgnoresGrantsThatAreNotEnforced(t *testing.T) {
	diags := destroyWarnings(grantReport{
		"already-going": grant("deleting", "", "10.0.3.1"),
		"never-landed":  grant("failed", "STORAGE_ERROR"),
		"unenforceable": grant("failed", "GRANT_ENFORCEMENT_UNAVAILABLE"),
		"still-coming":  grant("creating", ""),
		"no-state":      grant("", "", "10.0.3.2"),
	})

	if diags.WarningsCount() != 0 {
		t.Errorf("warned about a grant nobody holds: %v", diags)
	}
}

// `creating` with addresses is either a first grant not yet applied or a
// routine repair, during which the old grant is still enforced. The two read
// the same, so it is listed apart, as access that may already be held.
func TestDestroyListsAGrantBeingAppliedAsUncertain(t *testing.T) {
	diags := destroyWarnings(grantReport{
		"inference-fleet": grant("active", "", "10.0.1.7"),
		"training-fleet":  grant("creating", "", "10.0.2.9"),
	})
	if diags.WarningsCount() != 1 {
		t.Fatalf("%d warnings, want 1: %v", diags.WarningsCount(), diags)
	}
	w := diags.Warnings()[0]
	if !strings.Contains(w.Summary(), "will cut off 1") {
		t.Errorf("summary counts the uncertain one as held: %q", w.Summary())
	}
	held, maybe, ok := strings.Cut(w.Detail(), "may already")
	if !ok {
		t.Fatalf("detail has no separate list for uncertain grants: %q", w.Detail())
	}
	if !strings.Contains(held, "inference-fleet") || strings.Contains(held, "training-fleet") {
		t.Errorf("held list wrong: %q", held)
	}
	if !strings.Contains(maybe, "training-fleet (10.0.2.9)") {
		t.Errorf("uncertain list wrong: %q", maybe)
	}

	only := destroyWarnings(grantReport{
		"training-fleet": grant("creating", "", "10.0.2.9"),
	})
	if only.WarningsCount() != 1 || !strings.Contains(only.Warnings()[0].Summary(), "may cut off 1") {
		t.Errorf("an uncertain grant alone is not reported as possible: %v", only)
	}
}

func TestDestroyIsSilentWhenNothingIsBound(t *testing.T) {
	if diags := destroyWarnings(grantReport{}); diags.WarningsCount() != 0 {
		t.Errorf("warned on an unbound store: %v", diags)
	}
}
