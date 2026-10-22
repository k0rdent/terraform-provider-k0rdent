package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// budget is what the nightly holds a run to. Three kinds of limit, chosen so
// a slower CI machine doesn't trip them and a real regression does.
//
// Requests per resource don't depend on the machine at all, so they're held
// closely: a change that makes refresh read twice per resource fails however
// fast the runner is.
//
// Growth is a ratio of the same run against itself: seconds per resource at
// the largest size over seconds per resource at the reference size. Linear
// work holds it near one on any machine. A lookup that lists every object for
// each object makes it grow with the size, and that's the quadratic shape
// this exists to catch.
//
// Seconds are the published limit's own figures with headroom for a shared
// runner, so they catch a regression too large to argue about rather than
// noise.
type budget struct {
	// Parallelism is the -parallelism every limit is checked at.
	Parallelism int `json:"parallelism"`

	// MaxRequestsPerResource is per command (plan, refresh, apply, destroy),
	// at every size measured.
	MaxRequestsPerResource map[string]float64 `json:"max_requests_per_resource"`

	// MaxListReads is per command, keyed by size as a string, then by
	// command. A list that's read once per object of some kind grows with the
	// estate, and the count at each size pins how many there are, so a new
	// list-all lookup per resource fails it.
	MaxListReads map[string]map[string]int `json:"max_list_reads"`

	// GrowthReference is the size that seconds per resource are compared
	// against, and MaxGrowth is the largest ratio allowed, per command.
	GrowthReference int                `json:"growth_reference"`
	MaxGrowth       map[string]float64 `json:"max_growth"`

	// MaxSeconds is per command, keyed by size as a string since JSON keys
	// are, then by command.
	MaxSeconds map[string]map[string]float64 `json:"max_seconds"`
}

func readBudget(path string) (budget, error) {
	var b budget
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, fmt.Errorf("%s: %w", path, err)
	}
	if b.Parallelism <= 0 {
		return b, fmt.Errorf("%s names no parallelism to check at", path)
	}
	return b, nil
}

// check answers every limit the results break, in words a failed nightly can
// be read from.
func (b budget) check(r results) []string {
	var out []string
	measured := false
	ref := map[string]measurement{}
	for _, m := range r.Measurements {
		if m.Parallelism != b.Parallelism {
			continue
		}
		measured = true
		if m.Size == b.GrowthReference {
			ref[m.Phase] = m
		}
		if limit, ok := b.MaxRequestsPerResource[m.Phase]; ok && m.PerResource > limit {
			out = append(out, fmt.Sprintf("%s at %d resources made %.2f requests a resource, over the %.2f budgeted",
				m.Phase, m.Size, m.PerResource, limit))
		}
		if limit, ok := b.MaxListReads[fmt.Sprint(m.Size)][m.Phase]; ok && m.Requests.ListReads > limit {
			out = append(out, fmt.Sprintf("%s at %d resources read %d lists, over the %d budgeted (%v)",
				m.Phase, m.Size, m.Requests.ListReads, limit, m.Requests.Lists))
		}
		if limit, ok := b.MaxSeconds[fmt.Sprint(m.Size)][m.Phase]; ok && m.Seconds > limit {
			out = append(out, fmt.Sprintf("%s at %d resources took %.2fs, over the %.0fs budgeted",
				m.Phase, m.Size, m.Seconds, limit))
		}
	}
	if !measured {
		return []string{fmt.Sprintf("nothing was measured at -parallelism=%d, which the budget is checked at", b.Parallelism)}
	}
	for _, m := range r.Measurements {
		limit, ok := b.MaxGrowth[m.Phase]
		base, haveBase := ref[m.Phase]
		if !ok || !haveBase || m.Parallelism != b.Parallelism || m.Size <= b.GrowthReference || base.Seconds <= 0 {
			continue
		}
		growth := (m.Seconds / float64(m.Size)) / (base.Seconds / float64(base.Size))
		if growth > limit {
			out = append(out, fmt.Sprintf("%s took %.2f times as long a resource at %d resources as at %d, over the %.2f budgeted: the work isn't linear in the configuration's size",
				m.Phase, growth, m.Size, base.Size, limit))
		}
	}
	return out
}
