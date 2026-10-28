package repocheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	topLevelReadOnly = regexp.MustCompile(`(?m)^permissions:\n  contents: read\n`)
	releaseDraft     = regexp.MustCompile(`(?m)^  draft: true$`)
	writePermission  = regexp.MustCompile(`(?m)^\s*(permissions:\s*write-all|[a-z-]+:\s*write)\s*$`)
	// The same grant written inline, `permissions: {contents: write}`, which
	// the line-by-line pattern above can't see.
	inlineWritePermission = regexp.MustCompile(`permissions:\s*\{[^}]*:\s*write\b`)

	releasePrerelease = regexp.MustCompile(`(?m)^  prerelease:[ \t]*(\S*)`)
)

// validateReleasePublishing keeps the GitHub Release, which the registries
// download from, behind the same checks as the signing. goreleaser makes a
// draft, and the signing job publishes it only after validating the
// artifacts and checking the draft holds them. That job is the only one in
// any workflow that can write, apart from two nightly jobs that write issues
// only: report, and signing-key-reminder.
func validateReleasePublishing(workflows map[string]string, goreleaser string) error {
	const (
		dispatcher = "release-dispatch.yml"
		signingJob = "build-signed-artifacts"
		nightly    = "nightly.yml"
	)
	// The nightly jobs that keep one issue open: report while the nightly
	// fails, signing-key-reminder while the signing key is due to rotate.
	nightlyIssueJobs := []string{"report", "signing-key-reminder"}

	release, ok := yamlBlock(goreleaser, "release")
	if !ok {
		return fmt.Errorf(".goreleaser.yaml has no release section, so no GitHub Release for the registries")
	}
	if strings.Contains(release, "disable: true") {
		return fmt.Errorf(".goreleaser.yaml disables the GitHub release")
	}
	if !releaseDraft.MatchString(release) {
		return fmt.Errorf(".goreleaser.yaml publishes the release before release-dispatch.yml validates it. Set draft: true")
	}
	if !strings.Contains(release, "terraform-registry-manifest.json") {
		return fmt.Errorf(".goreleaser.yaml leaves the registry manifest off the release")
	}
	// registry.terraform.io ignores a GitHub release marked pre-release, so a
	// candidate marked one never reaches it.
	if m := releasePrerelease.FindStringSubmatch(release); m != nil && m[1] != "false" {
		return fmt.Errorf(".goreleaser.yaml sets prerelease: %s, and registry.terraform.io ignores a release marked pre-release", m[1])
	}

	for name, workflow := range workflows {
		if !topLevelReadOnly.MatchString(workflow) {
			return fmt.Errorf("%s doesn't start from permissions: contents: read", name)
		}
		outside := workflow
		if name == dispatcher {
			if job, found := workflowJobBlock(workflow, signingJob); found {
				outside = strings.Replace(workflow, job, "", 1)
			}
		}
		// Each writes issues and nothing else, so it can't touch a release.
		if name == nightly {
			for _, issueJob := range nightlyIssueJobs {
				if job, found := workflowJobBlock(workflow, issueJob); found {
					grants := writePermission.FindAllString(job, -1)
					if len(grants) == 1 && strings.TrimSpace(grants[0]) == "issues: write" {
						outside = strings.Replace(outside, job, "", 1)
					}
				}
			}
		}
		if m := writePermission.FindString(outside); m != "" {
			return fmt.Errorf("%s grants %q outside the %s job", name, strings.TrimSpace(m), signingJob)
		}
		if m := inlineWritePermission.FindString(workflow); m != "" {
			return fmt.Errorf("%s grants write inline as %q, which this check can't scope to a job; write it as a block", name, m)
		}
		if strings.Contains(workflow, "pull_request_target") {
			return fmt.Errorf("%s runs on pull_request_target, which hands a fork's code this repository's token", name)
		}
	}

	job, ok := workflowJobBlock(workflows[dispatcher], signingJob)
	if !ok {
		return fmt.Errorf("%s has no %s job", dispatcher, signingJob)
	}
	validate := strings.Index(job, "- name: Validate release artifact layout")
	check := strings.Index(job, "- name: Check the draft holds the validated files")
	publish := strings.Index(job, "-F draft=false")
	switch {
	case publish == -1:
		return fmt.Errorf("%s never publishes the draft release", signingJob)
	case validate == -1 || validate > publish:
		return fmt.Errorf("%s publishes the release before validating the artifacts", signingJob)
	case check == -1 || check > publish:
		return fmt.Errorf("%s publishes the release without checking the draft holds the validated files", signingJob)
	}
	if !strings.Contains(job, "sha256sum --check --strict") {
		return fmt.Errorf("%s doesn't compare the draft's files with dist/", signingJob)
	}
	// The tag's .goreleaser.yaml can predate the fix, as a release branch's
	// does, so the publish clears the flag itself.
	publishLine := job[publish:]
	if end := strings.IndexByte(publishLine, '\n'); end != -1 {
		publishLine = publishLine[:end]
	}
	if !strings.Contains(publishLine, "-F prerelease=false") {
		return fmt.Errorf("%s publishes without -F prerelease=false, so a draft marked pre-release stays one and registry.terraform.io ignores it", signingJob)
	}
	return nil
}

// yamlBlock returns a top-level key's block: its line and every indented
// line after it.
func yamlBlock(doc, key string) (string, bool) {
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if line != key+":" {
			continue
		}
		end := i + 1
		for end < len(lines) && (lines[end] == "" || strings.HasPrefix(lines[end], " ") || strings.HasPrefix(lines[end], "#")) {
			end++
		}
		return strings.Join(lines[i:end], "\n"), true
	}
	return "", false
}

func releasePublishingInputs(t *testing.T) (map[string]string, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return releaseSigningTestWorkflows(t), string(raw)
}

func TestReleasePublishingAcceptsCurrentConfig(t *testing.T) {
	workflows, goreleaser := releasePublishingInputs(t)
	if err := validateReleasePublishing(workflows, goreleaser); err != nil {
		t.Fatal(err)
	}
}

func TestReleasePublishingRejectsRegressions(t *testing.T) {
	replace := func(file, old, new string) func(map[string]string, *string) {
		return func(w map[string]string, g *string) {
			if file == ".goreleaser.yaml" {
				// Within the release section, since the checksum section
				// names the manifest the same way.
				head, tail, _ := strings.Cut(*g, "\nrelease:\n")
				*g = head + "\nrelease:\n" + strings.Replace(tail, old, new, 1)
				return
			}
			w[file] = strings.Replace(w[file], old, new, 1)
		}
	}
	tests := []struct {
		name   string
		mutate func(map[string]string, *string)
		want   string
	}{
		{"release turned off", replace(".goreleaser.yaml", "draft: true", "disable: true"), "disables the GitHub release"},
		{"published straight away", replace(".goreleaser.yaml", "draft: true", "draft: false"), "Set draft: true"},
		{"manifest left off", replace(".goreleaser.yaml", "  extra_files:\n    - glob: terraform-registry-manifest.json", "  extra_files:\n    - glob: other.json"), "leaves the registry manifest off"},
		{"write token for every job", replace("ci.yml", "permissions:\n  contents: read\n", "permissions:\n  contents: write\n"), "doesn't start from permissions: contents: read"},
		{"write token on another job", replace("ci.yml", "  build:\n    name: build\n", "  build:\n    name: build\n    permissions:\n      contents: write\n"), `grants "contents: write" outside`},
		{"issues token on another job", replace("ci.yml", "  build:\n    name: build\n", "  build:\n    name: build\n    permissions:\n      issues: write\n"), `grants "issues: write" outside`},
		{"inline write token on another job", replace("ci.yml", "  build:\n    name: build\n", "  build:\n    name: build\n    permissions: {contents: write}\n"), "grants write inline"},
		{"contents token on the nightly report", replace("nightly.yml", "    permissions:\n      actions: read\n      issues: write\n", "    permissions:\n      actions: read\n      issues: write\n      contents: write\n"), `grants "issues: write" outside`},
		{"contents token on the rotation reminder", replace("nightly.yml", "    permissions:\n      issues: write\n    steps:\n      - uses: actions/checkout", "    permissions:\n      issues: write\n      contents: write\n    steps:\n      - uses: actions/checkout"), `grants "issues: write" outside`},
		{"pull-requests token on the rotation reminder", replace("nightly.yml", "    permissions:\n      issues: write\n    steps:\n      - uses: actions/checkout", "    permissions:\n      issues: write\n      pull-requests: write\n    steps:\n      - uses: actions/checkout"), `grants "issues: write" outside`},
		{"issues token on the signing key check", replace("nightly.yml", "    name: signing key dates\n    runs-on: ubuntu-latest\n", "    name: signing key dates\n    runs-on: ubuntu-latest\n    permissions:\n      issues: write\n"), `grants "issues: write" outside`},
		{"issues token on a renamed reminder", replace("nightly.yml", "  signing-key-reminder:\n", "  signing-key-reminders:\n"), `grants "issues: write" outside`},
		{"pull_request_target", replace("changelog.yml", "  pull_request:\n", "  pull_request_target:\n"), "pull_request_target"},
		{"never published", replace("release-dispatch.yml", "-F draft=false", "-F draft=true"), "never publishes"},
		{"published before validation", replace("release-dispatch.yml", "- name: Validate release artifact layout", "- name: Validate later"), "before validating"},
		{"published without the draft check", replace("release-dispatch.yml", "- name: Check the draft holds the validated files", "- name: Look at the draft"), "without checking the draft"},
		{"draft not compared", replace("release-dispatch.yml", "sha256sum --check --strict", "true"), "doesn't compare the draft's files"},
		{"candidates marked pre-release", replace(".goreleaser.yaml", "  draft: true\n", "  draft: true\n  prerelease: auto\n"), "registry.terraform.io ignores a release marked pre-release"},
		{"every release marked pre-release", replace(".goreleaser.yaml", "  draft: true\n", "  draft: true\n  prerelease: true\n"), "registry.terraform.io ignores a release marked pre-release"},
		{"pre-release flag kept at publish", replace("release-dispatch.yml", " -F prerelease=false", ""), "publishes without -F prerelease=false"},
		{"pre-release flag set at publish", replace("release-dispatch.yml", "-F prerelease=false", "-F prerelease=true"), "publishes without -F prerelease=false"},
		{"pre-release flag cleared outside the publish", func(w map[string]string, g *string) {
			replace("release-dispatch.yml", " -F prerelease=false", "")(w, g)
			replace("release-dispatch.yml", "latest=false\n", "latest=false\n          echo -F prerelease=false\n")(w, g)
		}, "publishes without -F prerelease=false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflows, goreleaser := releasePublishingInputs(t)
			before := fmt.Sprint(workflows) + goreleaser
			tt.mutate(workflows, &goreleaser)
			if fmt.Sprint(workflows)+goreleaser == before {
				t.Fatal("the mutation matched nothing, so it tests nothing")
			}
			err := validateReleasePublishing(workflows, goreleaser)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateReleasePublishing() error = %v, want %q", err, tt.want)
			}
		})
	}
}
