package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The size asked for is the number of resources Terraform manages, exactly,
// and the mix holds at every size: a figure "at 1000 resources" is at 1000.
func TestTheEstateHoldsExactlyTheSizeAskedFor(t *testing.T) {
	for _, size := range []int{10, 100, 500, 1000} {
		e, err := generate(size, "100ms")
		if err != nil {
			t.Fatal(err)
		}
		blocks := regexp.MustCompile(`(?m)^resource "`).FindAllStringIndex(e.hcl, -1)
		if len(blocks) != size || e.kinds.total() != size {
			t.Errorf("size %d: %d resource blocks, %d counted", size, len(blocks), e.kinds.total())
		}
		if got, want := e.kinds["network"], size/5; got != want {
			t.Errorf("size %d: %d networks, want %d", size, got, want)
		}
		if got, want := e.kinds["subnet"], size*3/10; got != want {
			t.Errorf("size %d: %d subnets, want %d", size, got, want)
		}
		if size >= 100 {
			for _, k := range []string{"kubernetes_cluster", "instance_group", "webhook_subscription"} {
				if got, want := e.kinds[k], size/100; got != want {
					t.Errorf("size %d: %d %s, want %d", size, got, k, want)
				}
			}
		}
	}
}

// A subnet names its network by reference, so the graph Terraform walks has
// the edges a real configuration has, and every one points at a network the
// configuration declares.
func TestEverySubnetReferencesADeclaredNetwork(t *testing.T) {
	e, err := generate(100, "100ms")
	if err != nil {
		t.Fatal(err)
	}
	network := provider.ResourceName("network")
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`resource "`+network+`" "([^"]+)"`).FindAllStringSubmatch(e.hcl, -1) {
		declared[m[1]] = true
	}
	refs := regexp.MustCompile(`network += `+network+`\.([^.]+)\.id`).FindAllStringSubmatch(e.hcl, -1)
	if len(refs) != e.kinds["subnet"] {
		t.Fatalf("%d subnets but %d network references", e.kinds["subnet"], len(refs))
	}
	for _, r := range refs {
		if !declared[r[1]] {
			t.Errorf("a subnet names network %q, which isn't declared", r[1])
		}
	}
}

func TestASizeThatIsntAMultipleOfTenIsRefused(t *testing.T) {
	for _, size := range []int{0, -10, 15} {
		if _, err := generate(size, "100ms"); err == nil {
			t.Errorf("size %d was accepted", size)
		}
	}
}

// Lists are counted apart from item reads, by the collection they read, and
// the mint, the harness routes and anything that isn't a request line are
// left out.
func TestRequestsAreCountedByKind(t *testing.T) {
	log := strings.Join([]string{
		`2026/10/11 09:00:00 POST /v1/regions/global/auth/token ua="t"`,
		`2026/10/11 09:00:00 GET /v1/regions/us-texas-1/projects/demo/networking/networks/n1 ua="t" auth=present`,
		`2026/10/11 09:00:00 GET /v1/regions/us-texas-1/projects/demo/networking/vpcs ua="t" auth=present`,
		`2026/10/11 09:00:00 GET /v1/regions/global/notifications/webhooks ua="t" auth=present`,
		`2026/10/11 09:00:00 GET /v1/regions/us-texas-1/projects/demo/compute/clusters/c1/kubeconfig ua="t" auth=present`,
		`2026/10/11 09:00:00 PATCH /v1/regions/us-texas-1/projects/demo/networking/networks/n1 ua="t" auth=present`,
		`2026/10/11 09:00:00 DELETE /v1/regions/us-texas-1/projects/demo/networking/networks/n1 ua="t" auth=present`,
		`2026/10/11 09:00:00 GET /_harness/ready ua="k0rdent-test-harness" auth=present`,
		`2026/10/11 09:00:00 fakeapi: refused a request for host evil.example`,
		``,
	}, "\n")
	c, err := countRequests(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if c.Total != 7 || c.Mints != 1 || c.ItemReads != 2 || c.ListReads != 2 || c.Writes != 2 {
		t.Errorf("counted %+v", c)
	}
	if c.Lists["vpcs"] != 1 || c.Lists["webhooks"] != 1 {
		t.Errorf("lists by collection: %v", c.Lists)
	}
}

func TestCPUTimeReadsBothSpellings(t *testing.T) {
	for in, want := range map[string]float64{
		"0:01.87":    1.87,   // macOS
		"12:03.50":   723.5,  // macOS, past a minute
		"00:00:07":   7,      // Linux
		"01:02:03":   3723,   // Linux, past an hour
		"2-00:00:01": 172801, // Linux, past a day
		"nonsense":   0,
	} {
		if got := cpuSeconds(in); got != want {
			t.Errorf("cpuSeconds(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPSRowsAreReadByTheBinaryAndThePid(t *testing.T) {
	out := []byte(`  101  2048 0:01.00 /run/plugins/terraform-provider-x -some-flag
  102  4096 0:02.00 /usr/bin/terraform plan
  103  8192 0:03.00 /other/terraform-provider-x
`)
	s := &sampler{provider: "/run/plugins/terraform-provider-x", terraform: 102}
	rows := parsePS(out)
	if len(rows) != 3 {
		t.Fatalf("read %d rows", len(rows))
	}
	s.observe(rows)
	prov, tf, cpu := s.figures()
	if prov != 2 || tf != 4 || cpu != 1 {
		t.Errorf("provider %v MiB, terraform %v MiB, provider cpu %v s; a provider from another path isn't this run's", prov, tf, cpu)
	}
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		in   []float64
		want float64
	}{{nil, 0}, {[]float64{3}, 3}, {[]float64{5, 1, 3}, 3}, {[]float64{4, 1, 3, 2}, 2.5}} {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The budget fails on each of the three things it holds a run to, and names
// the figure that broke it.
func TestTheBudgetNamesEachBreach(t *testing.T) {
	b := budget{
		Parallelism:            10,
		MaxRequestsPerResource: map[string]float64{"plan": 1.2},
		MaxListReads:           map[string]map[string]int{"1000": {"plan": 22}},
		GrowthReference:        100,
		MaxGrowth:              map[string]float64{"plan": 2},
		MaxSeconds:             map[string]map[string]float64{"1000": {"plan": 15}},
	}
	plan := func(size int, secs, perResource float64, lists int) measurement {
		return measurement{Size: size, Parallelism: 10, Phase: "plan", Seconds: secs, PerResource: perResource,
			Requests: requestCounts{ListReads: lists, Lists: map[string]int{"webhooks": lists}}}
	}
	within := results{Measurements: []measurement{plan(100, 0.3, 1.02, 3), plan(1000, 2, 1.01, 21)}}
	if got := b.check(within); len(got) != 0 {
		t.Fatalf("a run within every limit breached: %v", got)
	}

	for name, c := range map[string]struct {
		m    measurement
		want string
	}{
		"a second read per resource": {plan(1000, 2, 2.01, 21), "requests a resource"},
		"a list per webhook again":   {plan(1000, 2, 1.01, 30), "lists"},
		"a quadratic plan":           {plan(1000, 9, 1.01, 21), "isn't linear"},
		"a plan past its limit":      {plan(1000, 16, 1.01, 21), "over the 15s budgeted"},
	} {
		t.Run(name, func(t *testing.T) {
			got := b.check(results{Measurements: []measurement{plan(100, 0.3, 1.02, 3), c.m}})
			if len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), c.want) {
				t.Errorf("breaches %q, want one saying %q", got, c.want)
			}
		})
	}

	if got := b.check(results{Measurements: []measurement{{Size: 100, Parallelism: 4, Phase: "plan"}}}); len(got) != 1 {
		t.Errorf("a run with nothing at the budget's parallelism passed: %v", got)
	}
}

// The committed budget reads, checks at the parallelism the nightly runs,
// and passes the figures docs/performance.md publishes for that parallelism.
func TestTheCommittedBudgetHoldsThePublishedFigures(t *testing.T) {
	b, err := readBudget("budget.json")
	if err != nil {
		t.Fatal(err)
	}
	if b.Parallelism != 10 {
		t.Errorf("the budget is checked at -parallelism=%d, and Terraform's default is 10", b.Parallelism)
	}
	// docs/performance.md's table at -parallelism=10, and the list reads the
	// request log recorded for the same run.
	published := []measurement{}
	for _, row := range []struct {
		size                          int
		plan, refresh, apply, destroy float64
		planLists, applyLists         int
		destroyLists                  int
	}{
		{10, 0.14, 0.14, 1.52, 0.85, 1, 5, 3},
		{100, 0.36, 0.25, 2.95, 2.51, 3, 8, 5},
		{500, 1.18, 0.69, 14.00, 11.56, 11, 32, 21},
		{1000, 2.22, 1.24, 29.01, 28.34, 21, 62, 41},
	} {
		perResource := map[string]float64{"plan": 1.02, "refresh": 1.02, "apply": 5.27, "destroy": 5.65}
		lists := map[string]int{"plan": row.planLists, "refresh": row.planLists, "apply": row.applyLists, "destroy": row.destroyLists}
		for phase, secs := range map[string]float64{"plan": row.plan, "refresh": row.refresh, "apply": row.apply, "destroy": row.destroy} {
			published = append(published, measurement{Size: row.size, Parallelism: 10, Phase: phase, Seconds: secs,
				PerResource: perResource[phase], Requests: requestCounts{ListReads: lists[phase]}})
		}
	}
	if breaches := b.check(results{Measurements: published}); len(breaches) != 0 {
		t.Errorf("the published figures break the committed budget: %v", breaches)
	}
}
