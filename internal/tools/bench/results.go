package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// measurement is one command at one size and one parallelism.
type measurement struct {
	Size        int           `json:"size"`
	Parallelism int           `json:"parallelism"`
	Phase       string        `json:"phase"`
	Seconds     float64       `json:"seconds"`
	Runs        []float64     `json:"runs"`
	Requests    requestCounts `json:"requests"`
	// PerResource is the requests the command made for each resource in the
	// configuration, mints left out. Flat across sizes is linear. Growing
	// with size is the quadratic shape.
	PerResource  float64 `json:"requests_per_resource"`
	ProviderMiB  float64 `json:"provider_peak_mib"`
	TerraformMiB float64 `json:"terraform_peak_mib"`
	// CPU seconds each side spent, median across the runs. Together they say
	// whose work the wall time is. TerraformCPU is the operating system's
	// account of the terraform process, which on Linux and macOS alike adds
	// in the children it reaped, so it's Terraform and the provider together,
	// exactly. ProviderCPU is the provider's share, a sampled floor. FakeCPU
	// is the fake's, the difference across the command.
	TerraformCPU float64 `json:"terraform_and_provider_cpu_seconds"`
	ProviderCPU  float64 `json:"provider_cpu_seconds"`
	FakeCPU      float64 `json:"fake_cpu_seconds"`
}

// environment is the machine the figures came from, stated beside them.
type environment struct {
	OS        string  `json:"os"`
	Arch      string  `json:"arch"`
	Kernel    string  `json:"kernel,omitempty"`
	CPU       string  `json:"cpu,omitempty"`
	CPUs      int     `json:"cpus"`
	MemoryGiB float64 `json:"memory_gib,omitempty"`
	Go        string  `json:"go"`
	Terraform string  `json:"terraform"`
}

type results struct {
	Machine      environment       `json:"machine"`
	Latency      string            `json:"latency"`
	PollInterval string            `json:"poll_interval"`
	Repeat       int               `json:"repeat"`
	Kinds        map[int]kindCount `json:"kinds,omitempty"`
	Measurements []measurement     `json:"measurements"`
}

func (r results) write(dir string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "results.json"), append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "results.md"), []byte(r.markdown()), 0o600)
}

// find answers the measurement for one phase at one size and parallelism.
func (r results) find(size, parallelism int, phase string) (measurement, bool) {
	for _, m := range r.Measurements {
		if m.Size == size && m.Parallelism == parallelism && m.Phase == phase {
			return m, true
		}
	}
	return measurement{}, false
}

// markdown is the table docs/performance.md publishes, one row per size and
// parallelism, one column group per command.
func (r results) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Machine: %s %s, %d CPUs", r.Machine.OS, r.Machine.Arch, r.Machine.CPUs)
	if r.Machine.CPU != "" {
		fmt.Fprintf(&b, " (%s)", r.Machine.CPU)
	}
	if r.Machine.MemoryGiB > 0 {
		fmt.Fprintf(&b, ", %.0f GiB", r.Machine.MemoryGiB)
	}
	fmt.Fprintf(&b, ". Terraform %s, %s. Fake latency %s a request, poll_interval %s, median of %d for plan and refresh.\n\n",
		r.Machine.Terraform, r.Machine.Go, r.Latency, r.PollInterval, r.Repeat)
	b.WriteString("| Resources | -parallelism | plan (s) | refresh (s) | apply (s) | destroy (s) | plan requests / resource | refresh requests / resource | apply requests / resource | list reads in a plan | provider peak (MiB) | Terraform peak (MiB) |\n")
	b.WriteString("| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	type key struct{ size, p int }
	var keys []key
	for _, m := range r.Measurements {
		k := key{m.Size, m.Parallelism}
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	slices.SortStableFunc(keys, func(a, c key) int {
		if a.size != c.size {
			return a.size - c.size
		}
		return a.p - c.p
	})
	for _, k := range keys {
		plan, _ := r.find(k.size, k.p, "plan")
		refresh, _ := r.find(k.size, k.p, "refresh")
		apply, _ := r.find(k.size, k.p, "apply")
		destroy, _ := r.find(k.size, k.p, "destroy")
		var provPeak, tfPeak float64
		for _, m := range []measurement{plan, refresh, apply, destroy} {
			provPeak, tfPeak = max(provPeak, m.ProviderMiB), max(tfPeak, m.TerraformMiB)
		}
		fmt.Fprintf(&b, "| %d | %d | %.2f | %.2f | %.2f | %.2f | %.2f | %.2f | %.2f | %d | %.0f | %.0f |\n",
			k.size, k.p, plan.Seconds, refresh.Seconds, apply.Seconds, destroy.Seconds,
			plan.PerResource, refresh.PerResource, apply.PerResource, plan.Requests.ListReads, provPeak, tfPeak)
	}

	b.WriteString("\nCPU seconds each side spent, at each size and -parallelism:\n\n")
	b.WriteString("| Resources | -parallelism | Command | Wall (s) | Terraform and provider CPU (s) | of which provider, sampled (s) | Fake CPU (s) |\n")
	b.WriteString("| ---: | ---: | --- | ---: | ---: | ---: | ---: |\n")
	for _, k := range keys {
		for _, phase := range []string{"plan", "refresh", "apply", "destroy"} {
			m, ok := r.find(k.size, k.p, phase)
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "| %d | %d | %s | %.2f | %.2f | %.2f | %.2f |\n",
				k.size, k.p, phase, m.Seconds, m.TerraformCPU, m.ProviderCPU, m.FakeCPU)
		}
	}
	return b.String()
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}
