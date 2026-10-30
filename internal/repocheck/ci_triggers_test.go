package repocheck

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// A release is tagged on a release branch, release-vMAJOR.MINOR.x, as often
// as on main. CI runs on every pull request, but only a push run checks the
// commit that actually lands, so ci.yml runs on pushes to both.
var ciPushBranches = []string{"main", "release-v*"}

func validateCIPushBranches(workflows map[string]string) error {
	var doc struct {
		On struct {
			Push struct {
				Branches []string `yaml:"branches"`
			} `yaml:"push"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal([]byte(workflows["ci.yml"]), &doc); err != nil {
		return fmt.Errorf("ci.yml: parse: %w", err)
	}
	for _, b := range ciPushBranches {
		if !slices.Contains(doc.On.Push.Branches, b) {
			return fmt.Errorf("ci.yml doesn't run on pushes to %s (push branches: %v)", b, doc.On.Push.Branches)
		}
	}
	return nil
}

func TestCIRunsOnPushesToReleaseBranches(t *testing.T) {
	if err := validateCIPushBranches(releaseCheckoutWorkflows(t)); err != nil {
		t.Fatal(err)
	}
}

func TestCIPushBranchesGuardRefusesDrift(t *testing.T) {
	cases := []struct {
		name, old, new, want string
	}{
		{"release branches dropped", `branches: [main, "release-v*"]`, "branches: [main]", "pushes to release-v*"},
		{"main dropped", `branches: [main, "release-v*"]`, `branches: ["release-v*"]`, "pushes to main"},
		{"push trigger dropped", "  push:\n    branches: [main, \"release-v*\"]\n", "", "pushes to main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			workflows := releaseCheckoutWorkflows(t)
			if !strings.Contains(workflows["ci.yml"], c.old) {
				t.Fatalf("ci.yml no longer holds the text this case edits")
			}
			workflows["ci.yml"] = strings.Replace(workflows["ci.yml"], c.old, c.new, 1)
			err := validateCIPushBranches(workflows)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}
