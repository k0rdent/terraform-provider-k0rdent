// Command bench measures plan, refresh and apply at realistic configuration
// sizes, against the local fake with a real stack's round trip injected, and
// holds the result to a budget. `make bench` runs it. docs/performance.md
// publishes what it measured, how, and on what.
//
// For each size and each -parallelism value it starts a fresh fake, writes a
// configuration of that many resources (config.go says what's in it), and
// times:
//
//	apply            the create of every resource, once
//	plan             a plan straight after, which has to be empty
//	refresh          `plan -refresh-only`, which has to be empty too
//	destroy          the teardown, once
//
// plan and refresh run -repeat times and the median is kept. For each command
// it records the fake's request log (requests.go), and the peak resident set
// of the provider process and of Terraform (memory.go).
//
// With -budget it then holds the figures to that file's limits (budget.go),
// prints every breach, and exits 1 if there is one. That's the nightly's
// regression gate.
//
// What it doesn't measure is the platform. The fake settles an object in two
// reads and answers from memory, so apply's figure is the provider's and
// Terraform's overhead plus the round trips, not the minutes a real cluster
// takes to come up. docs/performance.md says so beside the numbers.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

type options struct {
	sizes        []int
	parallelism  []int
	latency      time.Duration
	pollInterval string
	repeat       int
	terraform    string
	out          string
	budget       string
	results      string
	keep         bool
}

func main() {
	var o options
	var sizes, parallelism string
	flag.StringVar(&sizes, "sizes", "10,100,500,1000", "comma-separated resource counts, each a multiple of ten")
	flag.StringVar(&parallelism, "parallelism", "10,4,32", "comma-separated -parallelism values; Terraform's default is 10")
	flag.DurationVar(&o.latency, "latency", 3*time.Millisecond, "the round trip the fake adds to every API request")
	flag.StringVar(&o.pollInterval, "poll-interval", "100ms", "the provider's poll_interval while a create or delete settles")
	flag.IntVar(&o.repeat, "repeat", 3, "how many times plan and refresh run; the median is kept")
	flag.StringVar(&o.terraform, "terraform", envOr("TF_BIN", "terraform"), "the Terraform binary")
	flag.StringVar(&o.out, "out", ".bench", "where results.json, results.md and each command's output go")
	flag.StringVar(&o.budget, "budget", "", "a budget file to hold the results to; a breach exits 1")
	flag.StringVar(&o.results, "results", "", "hold an earlier run's results.json to -budget rather than measuring again")
	flag.BoolVar(&o.keep, "keep", false, "keep the run directory, with every configuration and log")
	flag.Parse()

	var err error
	if o.sizes, err = ints(sizes); err != nil {
		log.Fatalf("bench: -sizes: %v", err)
	}
	if o.parallelism, err = ints(parallelism); err != nil {
		log.Fatalf("bench: -parallelism: %v", err)
	}
	if o.results != "" {
		err = recheck(o)
	} else {
		err = run(o)
	}
	if err != nil {
		log.Fatalf("bench: %v", err)
	}
}

// recheck holds a results file an earlier run wrote to the budget, so a
// budget change can be tried against figures already measured.
func recheck(o options) error {
	if o.budget == "" {
		return errors.New("-results needs -budget to check them against")
	}
	raw, err := os.ReadFile(o.results)
	if err != nil {
		return err
	}
	var res results
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("%s: %w", o.results, err)
	}
	return holdToBudget(res, o.budget)
}

func run(o options) error {
	tf, err := exec.LookPath(o.terraform)
	if err != nil {
		return fmt.Errorf("find %s: %w", o.terraform, err)
	}
	if err = os.MkdirAll(o.out, 0o700); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "k0rdent-bench-")
	if err != nil {
		return err
	}
	if !o.keep {
		defer os.RemoveAll(work)
	} else {
		log.Printf("run directory: %s", work)
	}

	bins, err := build(work)
	if err != nil {
		return err
	}
	env, err := machine(tf)
	if err != nil {
		return err
	}
	res := results{Machine: env, Latency: o.latency.String(), PollInterval: o.pollInterval, Repeat: o.repeat}

	for _, size := range o.sizes {
		e, err := generate(size, o.pollInterval)
		if err != nil {
			return err
		}
		for _, p := range o.parallelism {
			log.Printf("%d resources, -parallelism=%d", size, p)
			ms, err := measure(o, tf, bins, work, e, p)
			res.Measurements = append(res.Measurements, ms...)
			if err != nil {
				// What was measured before the failure is still worth reading.
				return errors.Join(fmt.Errorf("%d resources at -parallelism=%d: %w", size, p, err), res.write(o.out))
			}
			for _, m := range ms {
				log.Printf("  %-8s %7.2fs  %5d requests (%.2f a resource, %d lists)  peak provider %.0f MiB, terraform %.0f MiB  cpu terraform and provider %.1fs (provider %.1fs), fake %.1fs",
					m.Phase, m.Seconds, m.Requests.Total, m.PerResource, m.Requests.ListReads, m.ProviderMiB, m.TerraformMiB,
					m.TerraformCPU, m.ProviderCPU, m.FakeCPU)
			}
		}
	}
	res.Kinds = map[int]kindCount{}
	for _, size := range o.sizes {
		e, err := generate(size, o.pollInterval)
		if err != nil {
			return err
		}
		res.Kinds[size] = e.kinds
	}
	if err := res.write(o.out); err != nil {
		return err
	}
	fmt.Print(res.markdown())

	if o.budget == "" {
		return nil
	}
	return holdToBudget(res, o.budget)
}

func holdToBudget(res results, path string) error {
	b, err := readBudget(path)
	if err != nil {
		return err
	}
	if breaches := b.check(res); len(breaches) > 0 {
		for _, br := range breaches {
			fmt.Fprintln(os.Stderr, "BUDGET:", br)
		}
		return fmt.Errorf("%d budget breaches against %s", len(breaches), path)
	}
	fmt.Printf("\nWithin every limit in %s.\n", path)
	return nil
}

// binaries is what one run builds: the fake, the provider, and the CLI
// configuration that points Terraform at the provider.
type binaries struct {
	fake, provider, cliConfig string
}

func build(work string) (binaries, error) {
	b := binaries{fake: filepath.Join(work, "bin", "fakeapi")}
	dir := filepath.Join(work, "plugins")
	b.provider = filepath.Join(dir, "terraform-provider-"+provider.ProviderTypeName)
	for _, step := range [][]string{
		{"go", "build", "-o", b.fake, "./internal/tools/fakeapi"},
		{"go", "build", "-ldflags", "-X main.version=0.0.0-bench", "-o", b.provider, "."},
	} {
		cmd := exec.Command(step[0], step[1:]...) //nolint:gosec // fixed argv
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return b, fmt.Errorf("%s: %w", strings.Join(step, " "), err)
		}
	}
	b.cliConfig = filepath.Join(work, "terraform.rc")
	rc := fmt.Sprintf("provider_installation {\n  dev_overrides {\n    %q = %q\n  }\n  direct {}\n}\n",
		provider.RegistryNamespace+"/"+provider.ProviderTypeName, dir)
	return b, os.WriteFile(b.cliConfig, []byte(rc), 0o600)
}

// measure runs one size at one parallelism against a fake of its own, so
// nothing one run made is in the next one's lists.
func measure(o options, tf string, bins binaries, work string, e estate, p int) ([]measurement, error) {
	dir := filepath.Join(work, fmt.Sprintf("n%d-p%d", e.size, p))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(e.hcl), 0o600); err != nil {
		return nil, err
	}
	f, err := startFake(bins.fake, dir, o.latency)
	if err != nil {
		return nil, err
	}
	defer f.stop()

	env := append(scrubbed(os.Environ()),
		"TF_CLI_CONFIG_FILE="+bins.cliConfig,
		"TF_IN_AUTOMATION=1",
		"CHECKPOINT_DISABLE=1",
		"TF_VAR_endpoint="+f.url,
		provider.EnvVar("CLIENT_ID")+"="+f.clientID,
		provider.EnvVar("CLIENT_SECRET")+"="+f.clientSecret,
	)
	par := "-parallelism=" + strconv.Itoa(p)
	steps := []struct {
		phase   string
		args    []string
		times   int
		wantOK0 bool
	}{
		{"apply", []string{"apply", "-auto-approve", "-input=false", "-no-color", par}, 1, false},
		{"plan", []string{"plan", "-detailed-exitcode", "-input=false", "-no-color", "-lock=false", par}, o.repeat, true},
		{"refresh", []string{"plan", "-refresh-only", "-detailed-exitcode", "-input=false", "-no-color", "-lock=false", par}, o.repeat, true},
		{"destroy", []string{"destroy", "-auto-approve", "-input=false", "-no-color", par}, 1, false},
	}
	var out []measurement
	for _, s := range steps {
		m := measurement{Size: e.size, Parallelism: p, Phase: s.phase}
		var cpu struct{ terraform, provider, fake []float64 }
		for i := range s.times {
			before, err := f.logLines()
			if err != nil {
				return out, err
			}
			logPath := filepath.Join(dir, fmt.Sprintf("%s-%d.out", s.phase, i))
			fakeCPU := cpuOf(f.cmd.Process.Pid)
			r, err := timed(tf, s.args, dir, env, bins.provider, logPath)
			if err != nil {
				return out, fmt.Errorf("%s: %w\n%s", s.phase, err, tail(logPath, 40))
			}
			if r.code != 0 {
				what := "failed"
				if r.code == 2 && s.wantOK0 {
					what = "proposed changes after an apply that changed nothing, so the plan isn't accurate"
				}
				return out, fmt.Errorf("terraform %s %s (exit %d)\n%s", s.phase, what, r.code, tail(logPath, 40))
			}
			counts, err := f.requestsSince(before)
			if err != nil {
				return out, err
			}
			m.Runs = append(m.Runs, r.seconds)
			m.Requests = counts
			m.ProviderMiB = max(m.ProviderMiB, r.providerMiB)
			m.TerraformMiB = max(m.TerraformMiB, r.terraformMiB)
			cpu.terraform = append(cpu.terraform, r.terraformCPU)
			cpu.provider = append(cpu.provider, r.providerCPU)
			cpu.fake = append(cpu.fake, cpuOf(f.cmd.Process.Pid)-fakeCPU)
		}
		m.Seconds = median(m.Runs)
		m.TerraformCPU, m.ProviderCPU, m.FakeCPU = median(cpu.terraform), median(cpu.provider), median(cpu.fake)
		m.PerResource = float64(m.Requests.Total-m.Requests.Mints) / float64(e.size)
		out = append(out, m)
	}
	return out, nil
}

// timed runs one Terraform command and answers its wall time, what the
// sampler saw of it, the CPU seconds Terraform and the providers it reaped
// used, and its exit code.
func timed(tf string, args []string, dir string, env []string, providerBin, logPath string) (commandRun, error) {
	logf, err := os.Create(logPath)
	if err != nil {
		return commandRun{}, err
	}
	defer logf.Close()
	cmd := exec.Command(tf, args...) //nolint:gosec // the Terraform binary the caller named
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, logf, logf

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	if err = cmd.Start(); err != nil {
		return commandRun{}, err
	}
	s := &sampler{provider: providerBin, terraform: cmd.Process.Pid, interval: 100 * time.Millisecond}
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	err = cmd.Wait()
	r := commandRun{seconds: time.Since(start).Seconds()}
	cancel()
	<-done

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.code, err = exit.ExitCode(), nil
	}
	r.providerMiB, r.terraformMiB, r.providerCPU = s.figures()
	if cmd.ProcessState != nil {
		r.terraformCPU = (cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()).Seconds()
	}
	return r, err
}

// commandRun is one Terraform command's figures.
type commandRun struct {
	seconds                   float64
	providerMiB, terraformMiB float64
	providerCPU, terraformCPU float64
	code                      int
}

// fake is one fakeapi process this run started and owns.
type fake struct {
	cmd                    *exec.Cmd
	url, log               string
	clientID, clientSecret string
}

func startFake(bin, dir string, latency time.Duration) (*fake, error) {
	f := &fake{log: filepath.Join(dir, "fake.log")}
	secrets := filepath.Join(dir, "fake-secrets.env")
	logf, err := os.Create(f.log)
	if err != nil {
		return nil, err
	}
	f.cmd = exec.Command(bin, "-secrets-file", secrets, "-latency", latency.String()) //nolint:gosec // built by this run
	f.cmd.Stderr = logf
	stdout, err := f.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = f.cmd.Start(); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		f.stop()
		return nil, fmt.Errorf("the fake printed no URL: %w", err)
	}
	f.url = strings.TrimSpace(line)
	go func() {
		if _, cerr := io.Copy(io.Discard, stdout); cerr != nil {
			log.Printf("bench: the fake's output: %v", cerr)
		}
	}()

	vals, err := readEnvFile(secrets)
	if err != nil {
		f.stop()
		return nil, err
	}
	f.clientID, f.clientSecret = vals["FAKE_CLIENT_ID"], vals["FAKE_CLIENT_SECRET"]
	if f.clientID == "" || f.clientSecret == "" {
		f.stop()
		return nil, errors.New("the fake's secrets file holds no client credential")
	}
	return f, nil
}

// stop kills the fake and waits for it. It was killed, so Wait answers that
// it exited on a signal, and only some other failure is worth reporting.
func (f *fake) stop() {
	if f.cmd.Process == nil {
		return
	}
	if err := f.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		log.Printf("bench: stop the fake: %v", err)
	}
	var exit *exec.ExitError
	if err := f.cmd.Wait(); err != nil && !errors.As(err, &exit) {
		log.Printf("bench: wait for the fake: %v", err)
	}
}

func (f *fake) logLines() (int, error) {
	b, err := os.ReadFile(f.log)
	return strings.Count(string(b), "\n"), err
}

// requestsSince counts the requests logged after the first skip lines.
func (f *fake) requestsSince(skip int) (requestCounts, error) {
	b, err := os.ReadFile(f.log)
	if err != nil {
		return requestCounts{}, err
	}
	lines := strings.SplitAfterN(string(b), "\n", skip+1)
	rest := ""
	if len(lines) > skip {
		rest = lines[skip]
	}
	return countRequests(strings.NewReader(rest))
}

// readEnvFile reads the KEY='value' lines the fake's secrets file holds.
func readEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vals := map[string]string{}
	for line := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && !strings.HasPrefix(k, "#") {
			vals[k] = strings.Trim(v, "'")
		}
	}
	return vals, nil
}

// scrubbed drops whatever the calling shell set for the provider or for
// Terraform's own behaviour, so an endpoint or a TF_LOG left exported
// can't point the run elsewhere or slow it down.
func scrubbed(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return strings.HasPrefix(kv, provider.EnvPrefix()) || strings.HasPrefix(kv, "TF_")
	})
}

// machine describes what the figures were measured on.
func machine(tf string) (environment, error) {
	v, err := exec.Command(tf, "version", "-json").Output() //nolint:gosec // the Terraform binary the caller named
	if err != nil {
		return environment{}, fmt.Errorf("%s version: %w", tf, err)
	}
	var tv struct {
		Version string `json:"terraform_version"`
	}
	if err = json.Unmarshal(v, &tv); err != nil {
		return environment{}, fmt.Errorf("%s version -json: %w", tf, err)
	}
	env := environment{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Go: runtime.Version(), Terraform: tv.Version}
	if b, err := exec.Command("uname", "-mrs").Output(); err == nil {
		env.Kernel = strings.TrimSpace(string(b))
	}
	if runtime.GOOS == "darwin" {
		if b, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			env.CPU = strings.TrimSpace(string(b))
		}
		if b, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
				env.MemoryGiB = float64(n) / (1 << 30)
			}
		}
	} else if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for line := range strings.Lines(string(b)) {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
				env.CPU = strings.TrimSpace(v)
				break
			}
		}
	}
	return env, nil
}

func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(the output in %s couldn't be read: %v)", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

func ints(s string) ([]int, error) {
	var out []int
	for f := range strings.SplitSeq(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%q isn't a positive whole number", f)
		}
		out = append(out, n)
	}
	return out, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
