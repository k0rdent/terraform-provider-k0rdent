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
	if !strings.Contains(dispatch, `history="$(git rev-list --first-parent "$2")"`) ||
		!strings.Contains(dispatch, `grep -qxF "$1" <<< "$history"`) ||
		!strings.Contains(dispatch, `if ! on_own_history "$tag_commit" origin/main; then`) {
		return fmt.Errorf("%s accepts a tag that isn't on main's own history", dispatcher)
	}
	// A tag off main is built only from its own minor's release branch, and
	// only once that branch is protected like main and holds nothing but
	// merged pull requests. Each piece is named, so removing one fails here.
	releaseBranchChecks := []struct{ text, what string }{
		{`release_branch="release-v${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.x"`, "its own minor's release branch"},
		{`on_own_history "$tag_commit" "origin/$release_branch"`, "the tag on the branch's own history"},
		{`rules/branches/$release_branch`, "the branch's rules"},
		{"for rule in pull_request required_signatures required_status_checks non_fast_forward deletion; do", "every rule main has, plus deletion"},
		{".parameters.required_approving_review_count", "at least one approval"},
		{`index("CI")`, "the CI check"},
		{`git merge-base origin/main "origin/$release_branch"`, "a branch point on main's own history"},
		{`on_own_history "$branch_point" origin/main`, "a branch point on main's own history"},
		{`commits/$commit/pulls`, "a pull request behind every commit"},
		{`merge_commit_sha == \"$commit\"`, "the pull request's own merge"},
		{`pulls/$number/reviews`, "an approval behind every pull request"},
		{`.user.login != \"$author\"`, "an approval from someone other than the author"},
	}
	for _, check := range releaseBranchChecks {
		if !strings.Contains(dispatch, check.text) {
			return fmt.Errorf("%s builds from a release branch without checking %s", dispatcher, check.what)
		}
	}
	// The major gate counts only releases this workflow published, so a
	// mistaken tag, which can never be deleted, doesn't count as a release.
	if !strings.Contains(dispatch, `check-major-release.sh "$RELEASE_TAG" "${published_tags[@]}"`) {
		return fmt.Errorf("%s doesn't give the major version gate the published releases", dispatcher)
	}
	if !strings.Contains(dispatch, `select((.draft | not) and .author.login == "github-actions[bot]")`) {
		return fmt.Errorf("%s counts a draft or a hand-made release as published", dispatcher)
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

	checkJob, err := validateSigningCheck(workflows)
	if err != nil {
		return err
	}

	for name, workflow := range workflows {
		outsideSigningJob := workflow
		switch name {
		case dispatcher:
			outsideSigningJob = strings.Replace(workflow, job, "", 1)
		case signingCheck:
			outsideSigningJob = strings.Replace(workflow, checkJob, "", 1)
		}
		for _, ref := range workflowSecretReferences(outsideSigningJob) {
			if ref.Dynamic {
				return fmt.Errorf("non-static secrets expression appears outside the %s and %s jobs in %s", signingJob, signingCheckJob, name)
			}
			if isReleaseSigningSecret(ref.Name) {
				return fmt.Errorf("%s appears outside the %s and %s jobs in %s", ref.Name, signingJob, signingCheckJob, name)
			}
		}
	}
	return nil
}

const (
	signingCheck    = "release-signing-check.yml"
	signingCheckJob = "check-signing-key"
)

var (
	// A trigger other than a dispatch would run the check, with the key, for
	// an event someone else controls.
	otherWorkflowTrigger = regexp.MustCompile(`(?m)^[[:space:]]*(push|pull_request|pull_request_target|schedule|workflow_run|workflow_call|repository_dispatch|issue_comment|release):`)
	actionUse            = regexp.MustCompile(`(?m)^[[:space:]]*-?[[:space:]]*uses:[[:space:]]*(\S+)(.*)$`)
	commitPin            = regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)
	versionComment       = regexp.MustCompile(`^\s+#\s*v[0-9]+\.[0-9]+\.[0-9]+`)
)

// validateSigningCheck holds release-signing-check.yml, the one other
// workflow that gets the signing key, to the release's own rules: dispatched
// by hand from main, gated by the release-signing environment, its actions
// pinned, and the key gone from the runner however the check ends. It returns
// the job that holds the key.
func validateSigningCheck(workflows map[string]string) (string, error) {
	check, ok := workflows[signingCheck]
	if !ok {
		return "", fmt.Errorf("missing signing check %s", signingCheck)
	}
	if !strings.Contains(check, "workflow_dispatch:") {
		return "", fmt.Errorf("%s is not manually dispatched", signingCheck)
	}
	if m := otherWorkflowTrigger.FindString(check); m != "" {
		return "", fmt.Errorf("%s runs on %s, and must only be dispatched", signingCheck, strings.TrimSpace(m))
	}

	guard, ok := workflowJobBlock(check, "require-main")
	if !ok || !strings.Contains(guard, `"$GITHUB_REF" != "refs/heads/main"`) {
		return "", fmt.Errorf("%s does not require execution from main", signingCheck)
	}
	if len(workflowSecretReferences(guard)) > 0 {
		return "", fmt.Errorf("%s gives its require-main job a secret", signingCheck)
	}

	job, ok := workflowJobBlock(check, signingCheckJob)
	if !ok {
		return "", fmt.Errorf("%s has no %s job", signingCheck, signingCheckJob)
	}
	if !strings.Contains(job, "needs: require-main") {
		return "", fmt.Errorf("%s must wait for the main check", signingCheckJob)
	}
	if !strings.Contains(job, "environment: release-signing") {
		return "", fmt.Errorf("%s is not gated by the release-signing environment", signingCheckJob)
	}
	if !strings.Contains(job, "./scripts/check-signing-key.sh secret docs/signing-key.asc") {
		return "", fmt.Errorf("%s doesn't check the key against docs/signing-key.asc", signingCheckJob)
	}
	for _, use := range actionUse.FindAllStringSubmatch(check, -1) {
		if strings.HasPrefix(use[1], "./") {
			continue
		}
		if !commitPin.MatchString(use[1]) || !versionComment.MatchString(use[2]) {
			return "", fmt.Errorf("%s uses %s, which is not a commit pin with a '# vX.Y.Z' comment", signingCheck, use[1])
		}
	}
	cleanups := []struct{ text, what string }{
		{`gpgconf --homedir "$RUNNER_TEMP/gnupg" --kill all`, "stop gpg-agent"},
		{`rm -rf "$RUNNER_TEMP/gnupg"`, "remove the keyring"},
		{`rm -f "$RUNNER_TEMP/gpg-passphrase"`, "remove the passphrase file"},
	}
	for _, cleanup := range cleanups {
		if !alwaysStepRuns(job, cleanup.text) {
			return "", fmt.Errorf("%s doesn't %s in an if: always() step", signingCheckJob, cleanup.what)
		}
	}
	return job, nil
}

// alwaysStepRuns reports whether job has a step marked if: always() that
// runs text.
func alwaysStepRuns(job, text string) bool {
	for _, step := range strings.Split(job, "\n      - ")[1:] {
		if strings.Contains(step, "if: always()") && strings.Contains(step, text) {
			return true
		}
	}
	return false
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
					`if ! on_own_history "$tag_commit" origin/main; then`,
					`if ! git merge-base --is-ancestor "$tag_commit" origin/main; then`,
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
			name: "release branch pull requests not checked for approval",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					`.user.login != \"$author\"`,
					`.user.login != \"\"`,
					1,
				)
			},
			want: "someone other than the author",
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
			name: "major gate given no published releases",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					`check-major-release.sh "$RELEASE_TAG" "${published_tags[@]}"`,
					`check-major-release.sh "$RELEASE_TAG"`,
					1,
				)
			},
			want: "doesn't give the major version gate the published releases",
		},
		{
			name: "major gate counts drafts",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					`select((.draft | not) and .author.login == "github-actions[bot]")`,
					`select(.author.login == "github-actions[bot]")`,
					1,
				)
			},
			want: "counts a draft or a hand-made release as published",
		},
		{
			name: "major gate counts hand-made releases",
			mutate: func(workflows map[string]string) {
				workflows["release-dispatch.yml"] = strings.Replace(
					workflows["release-dispatch.yml"],
					`select((.draft | not) and .author.login == "github-actions[bot]")`,
					`select(.draft | not)`,
					1,
				)
			},
			want: "counts a draft or a hand-made release as published",
		},
		{
			name: "signing check without protected environment",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = strings.Replace(
					workflows["release-signing-check.yml"], "environment: release-signing", "environment: release-build", 1)
			},
			want: "check-signing-key is not gated by the release-signing environment",
		},
		{
			name: "signing check runs from any branch",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = strings.Replace(
					workflows["release-signing-check.yml"], `"$GITHUB_REF" != "refs/heads/main"`, `-z "$GITHUB_REF"`, 1)
			},
			want: "release-signing-check.yml does not require execution from main",
		},
		{
			name: "signing check doesn't wait for the main check",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = strings.Replace(
					workflows["release-signing-check.yml"], "    needs: require-main\n", "", 1)
			},
			want: "must wait for the main check",
		},
		{
			name: "signing check on a schedule",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = strings.Replace(
					workflows["release-signing-check.yml"], "  workflow_dispatch:\n", "  workflow_dispatch:\n  schedule:\n    - cron: \"0 0 * * *\"\n", 1)
			},
			want: "must only be dispatched",
		},
		{
			name: "signing check on pull requests",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = strings.Replace(
					workflows["release-signing-check.yml"], "  workflow_dispatch:\n", "  workflow_dispatch:\n  pull_request:\n", 1)
			},
			want: "must only be dispatched",
		},
		{
			name: "signing check action on a tag",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = regexp.MustCompile(`actions/checkout@[0-9a-f]{40}\s+# v[0-9.]+`).
					ReplaceAllString(workflows["release-signing-check.yml"], "actions/checkout@v7")
			},
			want: "uses actions/checkout@v7, which is not a commit pin",
		},
		{
			name: "signing check keeps its keyring",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = strings.Replace(
					workflows["release-signing-check.yml"], `rm -rf "$RUNNER_TEMP/gnupg"`, "true", 1)
			},
			want: "doesn't remove the keyring in an if: always() step",
		},
		{
			name: "signing check cleans up only on success",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = strings.Replace(
					workflows["release-signing-check.yml"], "- name: Remove the signing keyring\n        if: always()", "- name: Remove the signing keyring\n        if: success()", 1)
			},
			want: "doesn't stop gpg-agent in an if: always() step",
		},
		{
			name: "signing check keeps the passphrase file",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = strings.Replace(
					workflows["release-signing-check.yml"], `rm -f "$RUNNER_TEMP/gpg-passphrase"`, "true", 1)
			},
			want: "doesn't remove the passphrase file in an if: always() step",
		},
		{
			name: "signing check against no committed key",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = strings.Replace(
					workflows["release-signing-check.yml"], "check-signing-key.sh secret docs/signing-key.asc", "check-signing-key.sh secret /dev/null", 1)
			},
			want: "doesn't check the key against docs/signing-key.asc",
		},
		{
			name: "signing check gives the main check a secret",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] = strings.Replace(
					workflows["release-signing-check.yml"], "      - name: Refuse a run from another branch\n", "      - name: Refuse a run from another branch\n        env:\n          K: ${{ secrets.TERRAFORM_PROVIDER_GPG_PRIVATE_KEY }}\n", 1)
			},
			want: "gives its require-main job a secret",
		},
		{
			name: "signing key in another job of the signing check",
			mutate: func(workflows map[string]string) {
				workflows["release-signing-check.yml"] += "\n  report:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ secrets.TERRAFORM_PROVIDER_GPG_PASSPHRASE }}\n"
			},
			want: "TERRAFORM_PROVIDER_GPG_PASSPHRASE appears outside the build-signed-artifacts and check-signing-key jobs in release-signing-check.yml",
		},
		{
			name: "signing check missing",
			mutate: func(workflows map[string]string) {
				delete(workflows, "release-signing-check.yml")
			},
			want: "missing signing check",
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
			want:       "TERRAFORM_PROVIDER_GPG_PRIVATE_KEY appears outside the build-signed-artifacts and check-signing-key jobs",
		},
		{
			name:       "single quoted index",
			expression: "${{ secrets['TERRAFORM_PROVIDER_GPG_PRIVATE_KEY'] }}",
			want:       "TERRAFORM_PROVIDER_GPG_PRIVATE_KEY appears outside the build-signed-artifacts and check-signing-key jobs",
		},
		{
			name:       "double quoted index",
			expression: `${{ secrets["TERRAFORM_PROVIDER_GPG_PRIVATE_KEY"] }}`,
			want:       "TERRAFORM_PROVIDER_GPG_PRIVATE_KEY appears outside the build-signed-artifacts and check-signing-key jobs",
		},
		{
			name:       "dynamic index",
			expression: "${{ secrets[env.NAME] }}",
			want:       "non-static secrets expression appears outside the build-signed-artifacts and check-signing-key jobs",
		},
		{
			name:       "whole secret context",
			expression: "${{ toJSON(secrets) }}",
			want:       "non-static secrets expression appears outside the build-signed-artifacts and check-signing-key jobs",
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

// validateRotationReminder keeps the nightly's rotation reminder wired: the
// signing-key job hands what's due to signing-key-reminder even when it
// fails, and the reminder runs on main, after it, with the job's result.
func validateRotationReminder(workflows map[string]string) error {
	const nightly = "nightly.yml"
	dates, ok := workflowJobBlock(workflows[nightly], "signing-key")
	if !ok {
		return fmt.Errorf("%s has no signing-key job", nightly)
	}
	for _, text := range []string{
		`SIGNING_KEY_DUE_FILE="$RUNNER_TEMP/due" make signing-key-check || status=$?`,
		`due: ${{ steps.dates.outputs.due }}`,
		`exit "$status"`,
	} {
		if !strings.Contains(dates, text) {
			return fmt.Errorf("signing-key doesn't hand what's due to the reminder: missing %s", text)
		}
	}
	reminder, ok := workflowJobBlock(workflows[nightly], "signing-key-reminder")
	if !ok {
		return fmt.Errorf("%s has no signing-key-reminder job", nightly)
	}
	for _, check := range []struct{ text, what string }{
		{"needs: signing-key", "wait for the signing key check"},
		{"if: ${{ always() && github.ref == 'refs/heads/main' }}", "run after a failed check, on main only"},
		{"DUE: ${{ needs.signing-key.outputs.due }}", "read what's due"},
		{"RESULT: ${{ needs.signing-key.result }}", "read the check's result"},
		{"./scripts/signing-key-reminder.sh", "run scripts/signing-key-reminder.sh"},
	} {
		if !strings.Contains(reminder, check.text) {
			return fmt.Errorf("signing-key-reminder doesn't %s", check.what)
		}
	}
	return nil
}

func TestRotationReminderAcceptsCurrentNightly(t *testing.T) {
	if err := validateRotationReminder(releaseSigningTestWorkflows(t)); err != nil {
		t.Fatal(err)
	}
}

func TestRotationReminderRejectsRegressions(t *testing.T) {
	tests := []struct{ name, old, new, want string }{
		{"due not written", `SIGNING_KEY_DUE_FILE="$RUNNER_TEMP/due" make signing-key-check`, "make signing-key-check", "missing SIGNING_KEY_DUE_FILE"},
		{"due not an output", "due: ${{ steps.dates.outputs.due }}", "due: ''", "missing due:"},
		{"near expiry passes", `exit "$status"`, "exit 0", `missing exit "$status"`},
		{"reminder skipped after a failed check", "if: ${{ always() && github.ref == 'refs/heads/main' }}\n    runs-on: ubuntu-latest\n    permissions:\n      issues: write", "if: ${{ github.ref == 'refs/heads/main' }}\n    runs-on: ubuntu-latest\n    permissions:\n      issues: write", "run after a failed check"},
		{"reminder doesn't read the due keys", "DUE: ${{ needs.signing-key.outputs.due }}", "DUE: ''", "read what's due"},
		{"reminder doesn't read the result", "RESULT: ${{ needs.signing-key.result }}", "RESULT: success", "read the check's result"},
		{"reminder runs nothing", "./scripts/signing-key-reminder.sh", "true", "run scripts/signing-key-reminder.sh"},
		{"reminder gone", "  signing-key-reminder:\n", "  other:\n", "no signing-key-reminder job"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflows := releaseSigningTestWorkflows(t)
			mutated := strings.Replace(workflows["nightly.yml"], tt.old, tt.new, 1)
			if mutated == workflows["nightly.yml"] {
				t.Fatal("the mutation matched nothing, so it tests nothing")
			}
			workflows["nightly.yml"] = mutated
			err := validateRotationReminder(workflows)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateRotationReminder() error = %v, want %q", err, tt.want)
			}
		})
	}
}
