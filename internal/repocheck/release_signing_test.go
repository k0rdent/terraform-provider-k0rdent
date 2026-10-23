package repocheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// releaseSigningSecretNames must never appear outside the signing job. Signing
// material only: the client read token is ordinary build input.
var releaseSigningSecretNames = []string{
	"TERRAFORM_PROVIDER_GPG_PRIVATE_KEY",
	"TERRAFORM_PROVIDER_GPG_KEY_FINGERPRINT",
	"TERRAFORM_PROVIDER_GPG_PASSPHRASE",
}

var (
	workflowTagTrigger     = regexp.MustCompile(`(?m)^[[:space:]]*tags:[[:space:]]*$`)
	secretDotReference     = regexp.MustCompile(`(?i)\bsecrets\s*\.\s*([A-Z_][A-Z0-9_-]*)\b`)
	secretBracketReference = regexp.MustCompile(`(?is)\bsecrets\s*\[\s*(.*?)\s*\]`)
	secretContextReference = regexp.MustCompile(`(?i)\bsecrets\b`)
)

type workflowSecretReference struct {
	Name    string
	Dynamic bool
}

func workflowSources(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read workflow directory: %w", err)
	}

	workflows := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".yml" && filepath.Ext(entry.Name()) != ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // directory is inside this repository
		if err != nil {
			return nil, fmt.Errorf("read workflow %s: %w", entry.Name(), err)
		}
		workflows[entry.Name()] = string(raw)
	}
	return workflows, nil
}

// validateProtectedReleaseSigning keeps release credentials tied to the
// workflow that starts from protected main. A tag is an artifact reference, not
// an authority to receive credentials: the dispatcher validates its ancestry
// before the environment-gated signing job checks out that commit.
func validateProtectedReleaseSigning(workflows map[string]string) error {
	const (
		dispatcher = "release-dispatch.yml"
		retired    = "release.yml"
		signingJob = "build-signed-artifacts"
	)

	dispatch, ok := workflows[dispatcher]
	if !ok {
		return fmt.Errorf("missing protected release dispatcher %s", dispatcher)
	}
	legacy, ok := workflows[retired]
	if !ok {
		return fmt.Errorf("missing retired tag workflow %s", retired)
	}
	if !strings.Contains(dispatch, "workflow_dispatch:") {
		return fmt.Errorf("%s is not manually dispatched", dispatcher)
	}
	if !strings.Contains(dispatch, "ref: main") || !strings.Contains(dispatch, `"$GITHUB_REF" != "refs/heads/main"`) {
		return fmt.Errorf("%s does not require execution from main", dispatcher)
	}
	if !strings.Contains(dispatch, `git rev-list --first-parent origin/main | grep -qxF "$tag_commit"`) {
		return fmt.Errorf("%s accepts a tag that isn't on main's own history", dispatcher)
	}
	// A tag off main is built only from its own minor's release branch, and
	// only once that branch is protected like main and holds nothing but
	// merged pull requests. Each piece is named, so removing one fails here.
	releaseBranchChecks := []struct{ text, what string }{
		{`release_branch="release-v${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.x"`, "its own minor's release branch"},
		{`git rev-list --first-parent "origin/$release_branch" | grep -qxF "$tag_commit"`, "the tag on the branch's own history"},
		{`rules/branches/$release_branch`, "the branch's rules"},
		{"for rule in pull_request required_signatures required_status_checks non_fast_forward deletion; do", "every rule main has, plus deletion"},
		{".parameters.required_approving_review_count", "at least one approval"},
		{`index("CI")`, "the CI check"},
		{`git merge-base origin/main "origin/$release_branch"`, "a branch point on main's own history"},
		{`commits/$commit/pulls`, "a pull request behind every commit"},
		{`merge_commit_sha == \"$commit\"`, "the pull request's own merge"},
	}
	for _, check := range releaseBranchChecks {
		if !strings.Contains(dispatch, check.text) {
			return fmt.Errorf("%s builds from a release branch without checking %s", dispatcher, check.what)
		}
	}
	if !strings.Contains(dispatch, `check-major-release.sh "$RELEASE_TAG" origin/main "${release_branches[@]}"`) {
		return fmt.Errorf("%s doesn't give the major version gate the release branches", dispatcher)
	}
	if strings.Contains(dispatch, "push:") || workflowTagTrigger.MatchString(dispatch) {
		return fmt.Errorf("%s must not be tag-triggered", dispatcher)
	}
	if !strings.Contains(legacy, "workflow_dispatch:") || !strings.Contains(legacy, "exit 1") {
		return fmt.Errorf("%s is not an inert retired workflow", retired)
	}
	if len(workflowSecretReferences(legacy)) > 0 || strings.Contains(legacy, "push:") || workflowTagTrigger.MatchString(legacy) {
		return fmt.Errorf("%s must not receive credentials or run for a tag", retired)
	}

	job, ok := workflowJobBlock(dispatch, signingJob)
	if !ok {
		return fmt.Errorf("%s has no %s job", dispatcher, signingJob)
	}
	if !strings.Contains(job, "needs: validate-release-tag") {
		return fmt.Errorf("%s must wait for tag validation", signingJob)
	}
	if !strings.Contains(job, "environment: release-signing") {
		return fmt.Errorf("%s is not gated by the release-signing environment", signingJob)
	}

	for _, secret := range releaseSigningSecretNames {
		if !containsStaticSecretReference(workflowSecretReferences(job), secret) {
			return fmt.Errorf("%s does not use %s", signingJob, secret)
		}
	}

	for name, workflow := range workflows {
		outsideSigningJob := workflow
		if name == dispatcher {
			outsideSigningJob = strings.Replace(workflow, job, "", 1)
		}
		for _, ref := range workflowSecretReferences(outsideSigningJob) {
			if ref.Dynamic {
				return fmt.Errorf("non-static secrets expression appears outside the %s job in %s", signingJob, name)
			}
			if isReleaseSigningSecret(ref.Name) {
				return fmt.Errorf("%s appears outside the %s job in %s", ref.Name, signingJob, name)
			}
		}
	}
	return nil
}

func workflowSecretReferences(workflow string) []workflowSecretReference {
	var refs []workflowSecretReference
	for _, expression := range githubActionsExpressions(workflow) {
		for _, match := range secretDotReference.FindAllStringSubmatch(expression, -1) {
			refs = append(refs, workflowSecretReference{Name: match[1]})
		}
		expression = secretDotReference.ReplaceAllString(expression, "")
		for _, match := range secretBracketReference.FindAllStringSubmatch(expression, -1) {
			key := strings.TrimSpace(match[1])
			if len(key) >= 2 && (key[0] == '\'' || key[0] == '"') && key[len(key)-1] == key[0] {
				refs = append(refs, workflowSecretReference{Name: key[1 : len(key)-1]})
				continue
			}
			refs = append(refs, workflowSecretReference{Dynamic: true})
		}
		expression = secretBracketReference.ReplaceAllString(expression, "")
		if secretContextReference.MatchString(expression) {
			refs = append(refs, workflowSecretReference{Dynamic: true})
		}
	}
	return refs
}

func githubActionsExpressions(workflow string) []string {
	var expressions []string
	for remainder := workflow; ; {
		start := strings.Index(remainder, "${{")
		if start == -1 {
			return expressions
		}
		remainder = remainder[start+3:]
		end := strings.Index(remainder, "}}")
		if end == -1 {
			return expressions
		}
		expressions = append(expressions, remainder[:end])
		remainder = remainder[end+2:]
	}
}

func containsStaticSecretReference(refs []workflowSecretReference, name string) bool {
	for _, ref := range refs {
		if !ref.Dynamic && strings.EqualFold(ref.Name, name) {
			return true
		}
	}
	return false
}

func isReleaseSigningSecret(name string) bool {
	for _, secret := range releaseSigningSecretNames {
		if strings.EqualFold(name, secret) {
			return true
		}
	}
	return false
}

func workflowJobBlock(workflow, name string) (string, bool) {
	lines := strings.Split(workflow, "\n")
	header := "  " + name + ":"
	start := -1
	for i, line := range lines {
		if line == header {
			start = i
			break
		}
	}
	if start == -1 {
		return "", false
	}

	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") &&
			strings.HasSuffix(strings.TrimSpace(line), ":") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

func TestProtectedReleaseSigningAcceptsCurrentWorkflows(t *testing.T) {
	if err := validateProtectedReleaseSigning(releaseSigningTestWorkflows(t)); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedReleaseSigningRejectsSecurityRegressions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{
			name: "restored tag sourced signing credential",
			mutate: func(workflows map[string]string) {
				workflows["release.yml"] += "\n# historical tag signer\n# ${{ secrets.TERRAFORM_PROVIDER_GPG_PRIVATE_KEY }}\n"
			},
			want: "must not receive credentials",
		},
		{
			name: "signing job without protected environment",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					"environment: release-signing",
					"environment: release-build",
					1,
				)
			},
			want: "is not gated by the release-signing environment",
		},
		{
			name: "tag accepted off main's own history",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					`git rev-list --first-parent origin/main | grep -qxF "$tag_commit"`,
					`git merge-base --is-ancestor "$tag_commit" origin/main`,
					1,
				)
			},
			want: "isn't on main's own history",
		},
		{
			name: "release branch accepted without its rules",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					"for rule in pull_request required_signatures required_status_checks non_fast_forward deletion; do",
					"for rule in pull_request non_fast_forward deletion; do",
					1,
				)
			},
			want: "every rule main has",
		},
		{
			name: "release branch accepted with no approval",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					".parameters.required_approving_review_count",
					".parameters.dismiss_stale_reviews_on_push",
					1,
				)
			},
			want: "at least one approval",
		},
		{
			name: "release branch commits not traced to pull requests",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					`commits/$commit/pulls`,
					`commits/$commit`,
					1,
				)
			},
			want: "a pull request behind every commit",
		},
		{
			name: "release branch cut from anywhere",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					`git merge-base origin/main "origin/$release_branch"`,
					`git rev-parse "origin/$release_branch"`,
					1,
				)
			},
			want: "a branch point on main's own history",
		},
		{
			name: "major gate blind to release branches",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					`check-major-release.sh "$RELEASE_TAG" origin/main "${release_branches[@]}"`,
					`check-major-release.sh "$RELEASE_TAG" origin/main`,
					1,
				)
			},
			want: "doesn't give the major version gate the release branches",
		},
		{
			name: "retired workflow triggered by tags",
			mutate: func(workflows map[string]string) {
				workflows["release.yml"] = strings.Replace(
					workflows["release.yml"],
					"workflow_dispatch:",
					"push:\n    tags:\n      - 'v*'",
					1,
				)
			},
			want: "is not an inert retired workflow",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflows := releaseSigningTestWorkflows(t)
			tt.mutate(workflows)

			err := validateProtectedReleaseSigning(workflows)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateProtectedReleaseSigning() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestProtectedReleaseSigningRejectsSecretExpressionsOutsideSigningJob(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		want       string
	}{
		{
			name:       "direct property",
			expression: "${{ secrets.TERRAFORM_PROVIDER_GPG_PRIVATE_KEY }}",
			want:       "TERRAFORM_PROVIDER_GPG_PRIVATE_KEY appears outside the build-signed-artifacts job",
		},
		{
			name:       "single quoted index",
			expression: "${{ secrets['TERRAFORM_PROVIDER_GPG_PRIVATE_KEY'] }}",
			want:       "TERRAFORM_PROVIDER_GPG_PRIVATE_KEY appears outside the build-signed-artifacts job",
		},
		{
			name:       "double quoted index",
			expression: `${{ secrets["TERRAFORM_PROVIDER_GPG_PRIVATE_KEY"] }}`,
			want:       "TERRAFORM_PROVIDER_GPG_PRIVATE_KEY appears outside the build-signed-artifacts job",
		},
		{
			name:       "dynamic index",
			expression: "${{ secrets[env.NAME] }}",
			want:       "non-static secrets expression appears outside the build-signed-artifacts job",
		},
		{
			name:       "whole secret context",
			expression: "${{ toJSON(secrets) }}",
			want:       "non-static secrets expression appears outside the build-signed-artifacts job",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflows := releaseSigningTestWorkflows(t)
			addReleaseExpressionOutsideSigningJob(workflows, tt.expression)

			err := validateProtectedReleaseSigning(workflows)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateProtectedReleaseSigning() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestProtectedReleaseSigningAllowsSecretExpressionsInsideSigningJob(t *testing.T) {
	expressions := []string{
		"${{ secrets.TERRAFORM_PROVIDER_GPG_PRIVATE_KEY }}",
		"${{ secrets['TERRAFORM_PROVIDER_GPG_PRIVATE_KEY'] }}",
		`${{ secrets["TERRAFORM_PROVIDER_GPG_PRIVATE_KEY"] }}`,
		"${{ secrets[env.NAME] }}",
		"${{ toJSON(secrets) }}",
	}

	for _, expression := range expressions {
		t.Run(expression, func(t *testing.T) {
			workflows := releaseSigningTestWorkflows(t)
			addReleaseExpressionInsideSigningJob(workflows, expression)

			if err := validateProtectedReleaseSigning(workflows); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func addReleaseExpressionOutsideSigningJob(workflows map[string]string, expression string) {
	workflows["release-dispatch.yml"] += "\n  report:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo " + expression + "\n"
}

func addReleaseExpressionInsideSigningJob(workflows map[string]string, expression string) {
	const marker = "      - name: Validate release artifact layout"
	workflows["release-dispatch.yml"] = strings.Replace(
		workflows["release-dispatch.yml"],
		marker,
		"      - run: echo "+expression+"\n"+marker,
		1,
	)
}

func releaseSigningTestWorkflows(t *testing.T) map[string]string {
	t.Helper()

	workflows, err := workflowSources(filepath.Join(repoRoot(t), ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	return workflows
}
