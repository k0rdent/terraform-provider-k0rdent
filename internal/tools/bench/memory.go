package main

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sampler watches the processes one Terraform command runs: Terraform itself,
// by its process id, and every provider process Terraform starts, by the path
// the binary was started from, which is a directory only this run uses.
//
// The provider is Terraform's child, started and stopped by Terraform, so
// this process can't wait on it and read its rusage. ps can see it, on Linux
// and macOS alike. Sampling misses whatever happens between the last sample
// and the process's exit, so the provider's figures are floors. One ps a
// tick for both, since ps itself costs CPU the benchmark would otherwise be
// timing.
type sampler struct {
	provider  string
	terraform int
	interval  time.Duration

	mu           sync.Mutex
	providerRSS  int64              // KiB, the largest seen
	terraformRSS int64              // KiB, the largest seen
	providerCPU  map[string]float64 // seconds, the last seen, by pid
}

func (s *sampler) run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		s.sample()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *sampler) sample() {
	out, err := exec.Command("ps", "-axo", "pid=,rss=,time=,args=").Output()
	if err == nil {
		s.observe(parsePS(out))
	}
}

// observe takes one tick's processes into account.
func (s *sampler) observe(rows []psRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.providerCPU == nil {
		s.providerCPU = map[string]float64{}
	}
	for _, p := range rows {
		switch {
		case p.args == s.provider:
			s.providerRSS = max(s.providerRSS, p.rssKiB)
			s.providerCPU[p.pid] = max(s.providerCPU[p.pid], p.cpu)
		case p.pid == strconv.Itoa(s.terraform):
			s.terraformRSS = max(s.terraformRSS, p.rssKiB)
		}
	}
}

// figures answers the peak resident sets in MiB, and the CPU seconds every
// provider process was seen to use.
func (s *sampler) figures() (providerMiB, terraformMiB, providerCPU float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.providerCPU {
		providerCPU += c
	}
	return float64(s.providerRSS) / 1024, float64(s.terraformRSS) / 1024, providerCPU
}

type psRow struct {
	pid    string
	rssKiB int64
	cpu    float64
	args   string // the command, its first word only
}

// parsePS reads `ps -o pid=,rss=,time=,args=` output.
func parsePS(out []byte) []psRow {
	var rows []psRow
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		rss, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		rows = append(rows, psRow{pid: f[0], rssKiB: rss, cpu: cpuSeconds(f[2]), args: f[3]})
	}
	return rows
}

// cpuSeconds reads ps's cumulative CPU time. macOS writes it as m:ss.ss, Linux
// as [[dd-]hh:]mm:ss, so it's read as colon-separated fields from the right,
// after any day count.
func cpuSeconds(s string) float64 {
	var days float64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.ParseFloat(d, 64)
		if err != nil {
			return 0
		}
		days, s = n, rest
	}
	total := days * 86400
	unit := 1.0
	parts := strings.Split(s, ":")
	for i := len(parts) - 1; i >= 0; i-- {
		n, err := strconv.ParseFloat(parts[i], 64)
		if err != nil {
			return 0
		}
		total += n * unit
		unit *= 60
	}
	return total
}

// cpuOf answers one process's cumulative CPU seconds, for the fake, which
// outlives every command and is read before and after each.
func cpuOf(pid int) float64 {
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	return cpuSeconds(strings.TrimSpace(string(out)))
}
