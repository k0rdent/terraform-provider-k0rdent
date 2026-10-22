package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// Consumer grants on a file store: what the platform's report on each grant
// asks of a plan or an update, the memory of re-sends the platform refused,
// the `consumers` request body, and the warnings a grant raises.
//
// The verdicts and the warnings work on plain Go values, so they can be tested
// without a schema. Only the adapters read framework types or private state:
// grantReportOf, configuredRefs, readRefused, writeRefused and the two
// warnOn functions the resource calls.

// grantStatus is the platform's report on one consumer.
type grantStatus struct {
	state  string
	reason string
	// addresses are the ones the grant resolved to, sorted.
	addresses []string
}

// grantReport is `consumer_status`, keyed by ref.
type grantReport map[string]grantStatus

// addressKey is what a refusal is remembered against: the addresses the
// group had when it was refused, in an order that does not depend on how
// they were read.
func (g grantStatus) addressKey() string {
	addrs := append([]string(nil), g.addresses...)
	sort.Strings(addrs)
	return strings.Join(addrs, ",")
}

// sortedRefs is the report's refs in order, so diagnostics come out the same
// way every run.
func (r grantReport) sortedRefs() []string {
	refs := make([]string, 0, len(r))
	for ref := range r {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

// grantReportOf decodes `consumer_status`. A null or unknown map is an empty
// report.
func grantReportOf(ctx context.Context, statuses types.Map, diags *diag.Diagnostics) grantReport {
	if statuses.IsNull() || statuses.IsUnknown() {
		return nil
	}
	var observed map[string]fileStoreConsumerStatusModel
	if d := statuses.ElementsAs(ctx, &observed, false); d.HasError() {
		diags.Append(d...)
		return nil
	}
	out := make(grantReport, len(observed))
	for ref, cs := range observed {
		var addrs []string
		if !cs.ResolvedAddresses.IsNull() && !cs.ResolvedAddresses.IsUnknown() {
			diags.Append(cs.ResolvedAddresses.ElementsAs(ctx, &addrs, false)...)
		}
		sort.Strings(addrs)
		out[ref] = grantStatus{state: cs.State.ValueString(), reason: cs.StateReason.ValueString(), addresses: addrs}
	}
	return out
}

// configuredRefs is the keys of `consumers`. An entry not yet known still has
// a key, and still gets a verdict.
func configuredRefs(consumers types.Map) []string {
	if consumers.IsNull() || consumers.IsUnknown() {
		return nil
	}
	refs := make([]string, 0, len(consumers.Elements()))
	for ref := range consumers.Elements() {
		refs = append(refs, ref)
	}
	return refs
}

// grantVerdicts is what the platform's report asks of the configured
// consumers.
type grantVerdicts struct {
	// resend are drifted groups whose entry goes again, which is the
	// contract's only way to grant a group's current members.
	resend []string
	// held are drifted groups whose re-send was refused, with addresses that
	// have not moved since. Proposing them again would be refused again.
	held []string
	// ungrantable are groups whose entry must not go again: the id may now
	// name machines the grant was never meant for.
	ungrantable []string
}

// judgeGrants reads the report against the configured refs and the re-sends
// already refused. An entry no longer configured is going anyway, so its
// verdict asks nothing.
func judgeGrants(report grantReport, configured []string, refused refusedResends) grantVerdicts {
	wanted := make(map[string]bool, len(configured))
	for _, ref := range configured {
		wanted[ref] = true
	}
	var v grantVerdicts
	for _, ref := range report.sortedRefs() {
		cs := report[ref]
		if !wanted[ref] || cs.state != string(k0rdent.FileStoreConsumerStatusStateFailed) {
			continue
		}
		switch cs.reason {
		case string(k0rdent.FileStoreConsumerStateReasonGROUPMEMBERSHIPDRIFTED):
			if seen, ok := refused[ref]; ok && seen == cs.addressKey() {
				v.held = append(v.held, ref)
				continue
			}
			v.resend = append(v.resend, ref)
		case string(k0rdent.FileStoreConsumerStateReasonGROUPNOLONGERGRANTABLE):
			v.ungrantable = append(v.ungrantable, ref)
		}
	}
	return v
}

// planDiagnostics is what a plan says about the verdicts: a warning for each
// held re-send, then an error for each ungrantable group, or a warning for
// each re-send the plan proposes when nothing stops it.
func (v grantVerdicts) planDiagnostics() diag.Diagnostics {
	var diags diag.Diagnostics
	for _, ref := range v.held {
		diags.AddAttributeWarning(path.Root("consumers").AtMapKey(ref),
			"A consumer group cannot be resolved again yet",
			fmt.Sprintf("The instance group %q has changed membership and the platform refused to "+
				"resolve it again. Its grant still covers the machines it was written against. "+
				"This plan does not propose the re-send again until the platform's report changes.", ref))
	}
	diags.Append(v.ungrantableErrors()...)
	if len(v.ungrantable) > 0 {
		return diags
	}
	for _, ref := range v.resend {
		// Local: the provider's reading of a verdict the API already reported.
		diags.AddAttributeWarning(path.Root("consumers").AtMapKey(ref),
			"A consumer group will be resolved again",
			fmt.Sprintf("The instance group %q has changed membership since its grant was written, so "+
				"machines that joined it cannot mount the store. This apply sends the entry again and "+
				"the platform grants the group's current members.", ref))
	}
	return diags
}

// ungrantableErrors refuses each ungrantable group. The plan raises it, and
// so does an update the plan could not check because the keys were unknown.
// Neither sends anything, so the two say the same thing.
//
// The update refuses whenever this does, even when the keys it resolved to
// equal state. A drifted group in the same map still asks for a re-send, and
// a re-send carries every entry, the ungrantable one with it.
func (v grantVerdicts) ungrantableErrors() diag.Diagnostics {
	var diags diag.Diagnostics
	for _, ref := range v.ungrantable {
		// Local: the provider's reading of a verdict the API already reported.
		diags.AddAttributeError(path.Root("consumers").AtMapKey(ref),
			"A consumer group can no longer be granted",
			fmt.Sprintf("The platform reports that the instance group %q is being deleted, or that its "+
				"id now names a different group than the one the grant came from. Sending the entry "+
				"again would grant whatever machines the id names now, so nothing is sent. Remove "+
				"the entry, or point it at the group that should hold the access.", ref))
	}
	return diags
}

// refusedResendWarning reports a re-send the platform refused while the rest
// of the change went through. reason is the platform's own message.
func refusedResendWarning(refs []string, reason string) diag.Diagnostics {
	var diags diag.Diagnostics
	// Local: the platform's refusal is reported by its message alone.
	diags.AddWarning("A consumer group could not be resolved again",
		fmt.Sprintf("Sending the drifted entries %q again was refused: %s Their grants still "+
			"cover the machines they were written against. Anything else in the change was "+
			"applied without them, and plans stop proposing the re-send until the "+
			"platform's report changes.", refs, reason))
	return diags
}

// refusalMessage is the platform's own message for a refusal, redacted.
func refusalMessage(resp *http.Response, body []byte) string {
	var apiErr *k0rdent.APIError
	if errors.As(k0rdent.CheckResponse(resp, body), &apiErr) && apiErr.Message != "" {
		return redactDiagnosticText(apiErr.Message) + "."
	}
	return "the platform gave no reason."
}

// refusedResends holds, per ref, the addresses a drifted group still had when
// sending it again was refused. It lives in private state between runs.
type refusedResends map[string]string

// remember adds refs to the refusals already recorded. A re-send carries
// every entry, so one group's refusal must not erase another's.
func (r refusedResends) remember(report grantReport, refs []string) {
	for _, ref := range refs {
		r[ref] = report[ref].addressKey()
	}
}

// forget drops the refs a re-send just carried successfully.
func (r refusedResends) forget(refs []string) {
	for _, ref := range refs {
		delete(r, ref)
	}
}

// refusedDriftKey is the private-state key refusedResends is kept under.
const refusedDriftKey = "refused_drift"

type privateState interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

type privateWriter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// readRefused loads the refusals from private state. Something unreadable
// there is treated as nothing refused, which at worst proposes a re-send the
// platform refuses once more.
func readRefused(ctx context.Context, private privateState, diags *diag.Diagnostics) refusedResends {
	marker := refusedResends{}
	if private == nil {
		return marker
	}
	raw, d := private.GetKey(ctx, refusedDriftKey)
	diags.Append(d...)
	if len(raw) > 0 && json.Unmarshal(raw, &marker) != nil {
		return refusedResends{}
	}
	return marker
}

// writeRefused stores the refusals, and clears the key once there are none.
func writeRefused(ctx context.Context, private privateWriter, marker refusedResends) diag.Diagnostics {
	if len(marker) == 0 {
		return private.SetKey(ctx, refusedDriftKey, nil)
	}
	var diags diag.Diagnostics
	raw, err := json.Marshal(marker)
	if err != nil {
		// Local: encoding the provider's own private state.
		diags.AddError("Could not record the refused re-send", err.Error())
		return diags
	}
	return append(diags, private.SetKey(ctx, refusedDriftKey, raw)...)
}

// warnOnConsumerFailures surfaces a grant that did not settle.
//
// A warning rather than an error. `GROUP_MEMBERSHIP_DRIFTED` means a group's
// membership moved and the platform's repair did not stick, while the grant
// still covers the old members. Failing the apply on it would fail on a
// scale-up, and saying nothing would leave a mount reaching fewer machines than
// its owner thinks.
func warnOnConsumerFailures(ctx context.Context, m fileStoreModel, diags *diag.Diagnostics) {
	// Only the state and the reason are read. The warnings never name an
	// address, so a set that fails to decode is no reason to drop them.
	if m.ConsumerStatus.IsNull() || m.ConsumerStatus.IsUnknown() {
		return
	}
	var statuses map[string]fileStoreConsumerStatusModel
	if diags.Append(m.ConsumerStatus.ElementsAs(ctx, &statuses, false)...); diags.HasError() {
		return
	}
	report := make(grantReport, len(statuses))
	for ref, cs := range statuses {
		report[ref] = grantStatus{state: cs.State.ValueString(), reason: cs.StateReason.ValueString()}
	}
	diags.Append(report.failureWarnings()...)
}

func (r grantReport) failureWarnings() diag.Diagnostics {
	const drifted = string(k0rdent.FileStoreConsumerStateReasonGROUPMEMBERSHIPDRIFTED)

	var diags diag.Diagnostics
	for _, ref := range r.sortedRefs() {
		cs := r[ref]
		if cs.state != string(k0rdent.FileStoreConsumerStatusStateFailed) {
			continue
		}
		if cs.reason == drifted {
			diags.AddWarning(
				"A consumer's instance group has changed membership",
				fmt.Sprintf("The grant for %q still covers the machines it was written against and "+
					"is still enforced, but the group's membership has moved since. The next plan "+
					"proposes sending the entry again, which grants the group's current members.",
					ref),
			)
			continue
		}
		diags.AddWarning(
			"A consumer's grant did not settle",
			redactDiagnosticText(fmt.Sprintf("The grant for %q is `failed`%s. The store itself is usable; this consumer "+
				"cannot mount it.", ref,
				stateDetail(nil, cs.reason))),
		)
	}
	return diags
}

// consumersRequest maps the configured grants onto the request body. It returns
// nil for an absent list so that a create omits the field, which is the
// documented way to ask for a store nobody can mount yet. Update supplies an
// empty list explicitly; see there for why the two differ.
func consumersRequest(in []keyedConsumer) *[]k0rdent.Consumer {
	if in == nil {
		return nil
	}
	out := make([]k0rdent.Consumer, 0, len(in))
	for _, c := range in {
		entry := k0rdent.Consumer{
			Ref:       c.ref,
			MountPath: optionalString(c.MountPath),
		}
		if t := optionalString(c.Type); t != nil {
			kind := k0rdent.ConsumerType(*t)
			entry.Type = &kind
		}
		if lvl := optionalString(c.AccessLevel); lvl != nil {
			level := k0rdent.ConsumerAccessLevel(*lvl)
			entry.AccessLevel = &level
		}
		if c.Network != nil {
			// Allocated from the field rather than written out: the generator
			// renders this as a pointer to an anonymous struct, and restating
			// the type means restating its json tags, which can drift. See
			// zeroPtr.
			entry.Network = zeroPtr(entry.Network)
			entry.Network.Type = k0rdent.ConsumerNetworkType(c.Network.Type.ValueString())
			entry.Network.MacAddress = optionalString(c.Network.MacAddress)
			entry.Network.Guid = optionalString(c.Network.GUID)
		}
		out = append(out, entry)
	}
	return &out
}

// warnOnDestroyingABoundStore names what a destroy takes away. A warning, not
// an error: destroying a store something mounts is sometimes exactly what was
// meant. It reads prior state, so it needs no request and reports grants as of
// the last refresh, the same basis as the rest of the plan.
func warnOnDestroyingABoundStore(ctx context.Context, state fileStoreModel, diags *diag.Diagnostics) {
	// Not worth failing a plan over: a status that cannot be read names nobody.
	var ignored diag.Diagnostics
	diags.Append(grantReportOf(ctx, state.ConsumerStatus, &ignored).destroyWarning(state.ID.ValueString())...)
}

func (r grantReport) destroyWarning(storeID string) diag.Diagnostics {
	var held, maybe []string
	for ref, cs := range r {
		line := ref
		if len(cs.addresses) > 0 {
			line = fmt.Sprintf("%s (%s)", ref, strings.Join(cs.addresses, ", "))
		}
		switch grantEnforced(cs) {
		case grantIsEnforced:
			held = append(held, line)
		case grantMayBeEnforced:
			maybe = append(maybe, line)
		}
	}
	if len(held) == 0 && len(maybe) == 0 {
		return nil
	}
	sort.Strings(held)
	sort.Strings(maybe)

	summary := fmt.Sprintf("Destroying %s will cut off %d consumer(s)", storeID, len(held))
	if len(held) == 0 {
		summary = fmt.Sprintf("Destroying %s may cut off %d consumer(s)", storeID, len(maybe))
	}
	var b strings.Builder
	if len(held) > 0 {
		fmt.Fprintf(&b, "These hold an enforced grant on the store as of the last refresh and will "+
			"lose access when it is destroyed:\n\n  %s\n\n", strings.Join(held, "\n  "))
	}
	if len(maybe) > 0 {
		fmt.Fprintf(&b, "These have a grant being applied, which may already be enforced: during a "+
			"routine repair the earlier grant stays in place while the entry reads `creating`:"+
			"\n\n  %s\n\n", strings.Join(maybe, "\n  "))
	}
	b.WriteString("The data on the store goes with it. A store is not recoverable once deleted, so " +
		"if this is a migration rather than a decommission, copy the data first.")
	// The addresses and the store's ID are the platform's words.
	var diags diag.Diagnostics
	diags.AddWarning(redactDiagnosticText(summary), redactDiagnosticText(b.String()))
	return diags
}

type grantEnforcement int

const (
	grantNotEnforced grantEnforcement = iota
	grantIsEnforced
	grantMayBeEnforced
)

// grantEnforced reads one consumer's grant against the FileStore contract.
//
// "A drift report never widens or narrows a grant": a `failed` verdict from
// the drift sweep writes only the state and the reason, and the sweep only
// examines entries that authored addresses. So GROUP_MEMBERSHIP_DRIFTED,
// GROUP_NO_LONGER_GRANTABLE ("the grant is not altered on its own"),
// GROUP_MEMBER_SET_EMPTY, GROUP_NO_READY_HOSTS and NETWORK_MATCHED_NO_INTERFACE
// on an entry with addresses leave that grant enforced. The same reasons with
// no addresses come from a first write that authored nothing.
//
// "While that repair is under way the entry reads `creating` with no reason at
// all", and a first grant not yet applied reads the same, so `creating` with
// addresses may or may not be enforced. GRANT_ENFORCEMENT_UNAVAILABLE is a
// consumer that "cannot be enforced". `deleting` is a revocation the
// configuration already asked for.
func grantEnforced(cs grantStatus) grantEnforcement {
	hasAddresses := len(cs.addresses) > 0
	switch cs.state {
	case string(k0rdent.FileStoreConsumerStatusStateActive):
		return grantIsEnforced
	case string(k0rdent.FileStoreConsumerStatusStateCreating):
		if hasAddresses {
			return grantMayBeEnforced
		}
	case string(k0rdent.FileStoreConsumerStatusStateFailed):
		switch cs.reason {
		case string(k0rdent.FileStoreConsumerStateReasonGROUPMEMBERSHIPDRIFTED),
			string(k0rdent.FileStoreConsumerStateReasonGROUPNOLONGERGRANTABLE),
			string(k0rdent.FileStoreConsumerStateReasonGROUPMEMBERSETEMPTY),
			string(k0rdent.FileStoreConsumerStateReasonGROUPNOREADYHOSTS),
			string(k0rdent.FileStoreConsumerStateReasonNETWORKMATCHEDNOINTERFACE):
			if hasAddresses {
				return grantIsEnforced
			}
		}
	}
	return grantNotEnforced
}
