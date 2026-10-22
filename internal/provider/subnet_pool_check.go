package provider

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A subnet that can't come out of its pool is refused at plan.
//
// The platform checks a subnet's prefix_length against the pool's
// minPrefixLength and maxPrefixLength at create, which is hours into an apply
// that creates a cluster first. The pool's read answers both bounds, and its
// prefixes say the largest block it could ever give, so a size outside either
// is refused here with the numbers.
//
// The pool doesn't report its free space, so a pool too full for the size
// still fails at create, with INSUFFICIENT_CAPACITY and the guidance in
// error_guidance.go. Nothing here guesses at it.

// addressPoolMemo is each pool as this provider process read it, so a plan
// with many subnets on one pool reads it once. A failed read isn't kept.
type addressPoolMemo struct {
	mu   sync.Mutex
	seen map[string]k0rdent.AddressPool
}

// addressPool reads a pool, once per process, for the plan-time check and for
// a subnet's read. It's false when the read failed or answered no pool.
func (m *Meta) addressPool(ctx context.Context, region, id string) (k0rdent.AddressPool, bool) {
	key := region + "/" + id
	m.addressPools.mu.Lock()
	defer m.addressPools.mu.Unlock()
	if got, ok := m.addressPools.seen[key]; ok {
		return got, true
	}
	got, err := m.Client.GetAddressPoolWithResponse(ctx, region, id)
	if err != nil || got.JSON200 == nil {
		fields := map[string]any{"region": region, "pool": id}
		if err != nil {
			fields["error"] = redactDiagnosticText(err.Error())
		}
		tflog.Debug(ctx, "could not read the subnet's address pool", fields)
		return k0rdent.AddressPool{}, false
	}
	if m.addressPools.seen == nil {
		m.addressPools.seen = map[string]k0rdent.AddressPool{}
	}
	m.addressPools.seen[key] = *got.JSON200
	return *got.JSON200, true
}

// checkSubnetFitsPool refuses a subnet create whose prefix_length its pool
// can't give. A subnet that exists keeps its size whatever the pool says now,
// so only a new one, or one being replaced onto another pool or size, asks.
// Any value unknown at plan, or a pool that can't be read, is left to the
// platform.
func checkSubnetFitsPool(ctx context.Context, meta *Meta, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if meta == nil || meta.Client == nil || req.Plan.Raw.IsNull() {
		return
	}
	var pool, region types.String
	var length types.Int64
	if resp.Plan.GetAttribute(ctx, path.Root("address_pool"), &pool).HasError() ||
		resp.Plan.GetAttribute(ctx, path.Root("prefix_length"), &length).HasError() ||
		resp.Plan.GetAttribute(ctx, path.Root("region"), &region).HasError() {
		return
	}
	if pool.IsUnknown() || pool.IsNull() || length.IsUnknown() || length.IsNull() {
		return
	}
	if !req.State.Raw.IsNull() {
		var priorPool types.String
		var priorLength types.Int64
		if req.State.GetAttribute(ctx, path.Root("address_pool"), &priorPool).HasError() ||
			req.State.GetAttribute(ctx, path.Root("prefix_length"), &priorLength).HasError() {
			return
		}
		if priorPool.Equal(pool) && priorLength.Equal(length) {
			return
		}
	}
	poolRegion, id, ok := addressPoolRef(pool.ValueString(), region)
	if !ok {
		return
	}
	got, ok := meta.addressPool(ctx, poolRegion, id)
	if !ok {
		return
	}
	// Local: decided from the pool's read, which the platform answered.
	resp.Diagnostics.Append(subnetPoolProblems(id, got, length.ValueInt64())...)
}

// addressPoolRef is the region and id a subnet's address_pool names: a URI
// the platform gave, or a bare id in the subnet's own region. Either form must
// be one plain path segment, since a URI's segments arrive unescaped.
func addressPoolRef(ref string, region types.String) (string, string, bool) {
	if p, ok := addressPoolRoute.params(ref); ok {
		if !pathSegment.MatchString(p["region"]) || !pathSegment.MatchString(p["id"]) {
			return "", "", false
		}
		return p["region"], p["id"], true
	}
	if pathSegment.MatchString(ref) && !region.IsUnknown() && region.ValueString() != "" {
		return region.ValueString(), ref, true
	}
	return "", "", false
}

// subnetPoolProblems is what's wrong with asking pool id for a /length,
// against the bounds and prefixes its read reports. A bound the read leaves
// out isn't checked.
func subnetPoolProblems(id string, pool k0rdent.AddressPool, length int64) diag.Diagnostics {
	var diags diag.Diagnostics
	at := path.Root("prefix_length")
	minLen, maxLen := pool.MinPrefixLength, pool.MaxPrefixLength
	if (minLen != nil && length < int64(*minLen)) || (maxLen != nil && length > int64(*maxLen)) {
		diags.AddAttributeError(at, "The subnet's prefix_length is outside its address pool's range",
			redactDiagnosticText(fmt.Sprintf("Address pool %q gives %s, and `prefix_length` is %d. Set "+
				"`prefix_length` %s, or choose an `address_pool` whose range includes /%d.",
				id, poolRange(minLen, maxLen), length, poolRangeAdvice(minLen, maxLen), length)))
		return diags
	}
	largest, ok := largestBlock(pool.Prefixes)
	if ok && length < int64(largest) {
		diags.AddAttributeError(at, "The subnet is larger than any prefix in its address pool",
			redactDiagnosticText(fmt.Sprintf("Address pool %q holds %s, so the largest subnet it can give "+
				"is a /%d, and `prefix_length` is %d. Set `prefix_length` to %d or more, or choose a larger "+
				"`address_pool`.", id, strings.Join(pool.Prefixes, ", "), largest, length, largest)))
	}
	return diags
}

// largestBlock is the shortest prefix length among a pool's prefixes, the
// largest block it could give. It's false when none parses.
func largestBlock(prefixes []string) (int, bool) {
	best, found := 0, false
	for _, s := range prefixes {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			continue
		}
		if !found || p.Bits() < best {
			best, found = p.Bits(), true
		}
	}
	return best, found
}

func poolRange(minLen, maxLen *int32) string {
	switch {
	case minLen != nil && maxLen != nil:
		return fmt.Sprintf("subnets from /%d to /%d", *minLen, *maxLen)
	case minLen != nil:
		return fmt.Sprintf("subnets of /%d or smaller", *minLen)
	default:
		return fmt.Sprintf("subnets of /%d or larger", *maxLen)
	}
}

func poolRangeAdvice(minLen, maxLen *int32) string {
	switch {
	case minLen != nil && maxLen != nil:
		return fmt.Sprintf("between %d and %d", *minLen, *maxLen)
	case minLen != nil:
		return fmt.Sprintf("to %d or more", *minLen)
	default:
		return fmt.Sprintf("to %d or less", *maxLen)
	}
}
