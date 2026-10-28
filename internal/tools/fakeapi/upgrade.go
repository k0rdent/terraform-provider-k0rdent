package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// upgradeCluster models `upgradeCluster`: one step to the immediate next
// `available` version of the bound ClusterType, admitted only from `active`.
// The checks run in the order the service runs them, because the order decides
// which status a request that breaks two rules gets.
func (s *state) upgradeCluster(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")

	var body struct {
		ClusterTypeVersionID *string `json:"clusterTypeVersionId"`
	}
	// The request body is optional.
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c, _, ok := clusterKind.find(w, r, s.in(r))
	if !ok {
		return
	}
	c.busyForUpgrade()
	// `failed` included: a failed upgrade is not retryable through this call.
	if c.state != "active" {
		apiError(w, http.StatusConflict, "CONFLICT", fmt.Sprintf("cluster %q is not ACTIVE", c.id))
		return
	}

	next, hasNext := nextAvailable(c.ct, c.version.version)
	target := next
	if body.ClusterTypeVersionID == nil {
		if !hasNext {
			apiError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", fmt.Sprintf(
				"cluster is already at clusterType version %d, its head's latest available version; no next version to upgrade to",
				c.version.version))
			return
		}
	} else {
		var found bool
		target, found = findVersion(c.ct, *body.ClusterTypeVersionID)
		if !found {
			// A version of another ClusterType is not found under this one.
			apiError(w, http.StatusNotFound, "NOT_FOUND", "clusterType version "+*body.ClusterTypeVersionID)
			return
		}
		if target.status != "available" {
			apiError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", fmt.Sprintf(
				"clusterType version %d is %s, not available", target.version, target.status))
			return
		}
		if target.bmaasOnly {
			apiError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", fmt.Sprintf(
				"clusterType version %d has no k8s configuration; cannot upgrade a Kubernetes cluster through a BMaaS-only version",
				target.version))
			return
		}
		if !hasNext {
			apiError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", fmt.Sprintf(
				"cluster is already at clusterType version %d, its head's latest available version; no next version to upgrade to",
				c.version.version))
			return
		}
		switch {
		case target.version <= c.version.version:
			apiError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", fmt.Sprintf(
				"clusterType version %d is not newer than the cluster's version %d; upgrade is forward-only",
				target.version, c.version.version))
			return
		case target.versionID != next.versionID:
			apiError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", fmt.Sprintf(
				"clusterType version %d is more than one available version ahead of %d; the next is %d",
				target.version, c.version.version, next.version))
			return
		}
	}
	if msg := outOfBounds(c.ct, target, c.pools); msg != "" {
		apiError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", msg)
		return
	}

	c.upgradeTo = &target
	c.stateMessage = ""
	c.state = "updating"
	c.reads = 0
	c.start("upgrading the cluster", "preparing-version", "applying-config", "rolling-control-plane", "rolling-workers")
	c.op.done = 1
	writeJSON(w, http.StatusOK, s.clusterBody(region, c, false))
}

// settleUpgrade ends an upgrade: bound to the target, or `failed` with the
// binding unchanged, since nothing new landed.
func settleUpgrade(c *cluster) {
	target := *c.upgradeTo
	c.upgradeTo = nil
	if target.upgradeFails {
		c.state = "failed"
		c.stateMessage = fmt.Sprintf("the upgrade to clusterType version %d did not reconcile", target.version)
		c.record("warn", "control-plane", "updating-cluster",
			fmt.Sprintf("control-plane replicas did not report ready at clusterType version %d", target.version))
		c.record("error", "cluster", "updating-cluster", c.stateMessage)
		return
	}
	c.version = target
	c.clusterType = versionRef(regionOf(c.clusterType), c.ct, target)
	// A pool the new version brings is created at its default.
	for _, p := range c.ct.pools {
		if _, held := c.pools[p.id]; held || !p.at(target.version) {
			continue
		}
		c.pools[p.id] = 0
		if p.def != nil {
			c.pools[p.id] = int(*p.def)
		}
	}
}

// regionOf reads the region back out of a resolved reference.
func regionOf(ref string) string {
	rest, _ := strings.CutPrefix(ref, "/v1/regions/")
	region, _, _ := strings.Cut(rest, "/")
	return region
}

func versionRef(region string, ct fakeClusterType, v fakeVersion) string {
	return clusterTypePath(region, ct.id) + "/versions/" + v.versionID
}

func findVersion(ct fakeClusterType, versionID string) (fakeVersion, bool) {
	for _, v := range ct.versions {
		if v.versionID == versionID {
			return v, true
		}
	}
	return fakeVersion{}, false
}

// headVersion is the version an unversioned reference binds.
func headVersion(ct fakeClusterType) (fakeVersion, bool) {
	for _, v := range ct.versions {
		if v.version == ct.version {
			return v, true
		}
	}
	return fakeVersion{}, false
}

// nextAvailable is the smallest `available` version above current, skipping
// any draft or deprecated one in between.
func nextAvailable(ct fakeClusterType, current int32) (fakeVersion, bool) {
	versions := append([]fakeVersion(nil), ct.versions...)
	sort.Slice(versions, func(i, j int) bool { return versions[i].version < versions[j].version })
	for _, v := range versions {
		if v.version > current && v.status == "available" {
			return v, true
		}
	}
	return fakeVersion{}, false
}

// outOfBounds reports the first pool whose count breaks the bounds declared at
// a version, or "" when every pool fits.
func outOfBounds(ct fakeClusterType, v fakeVersion, counts map[string]int) string {
	for _, p := range ct.pools {
		count, ok := counts[p.id]
		if !ok {
			continue
		}
		min, max := p.min, p.max
		if m, moved := v.poolMin[p.id]; moved {
			min = &m
		}
		if m, moved := v.poolMax[p.id]; moved {
			max = &m
		}
		if (min != nil && count < int(*min)) || (max != nil && count > int(*max)) {
			return fmt.Sprintf("clusterType %q version %d: node pool %q count %d is outside [%s, %s]",
				ct.id, v.version, p.id, count, bound(min), bound(max))
		}
	}
	return ""
}

func bound(v *int32) string {
	if v == nil {
		return "unbounded"
	}
	return fmt.Sprint(*v)
}
