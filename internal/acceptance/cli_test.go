package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The suite runs under Terraform or OpenTofu, whichever TF_ACC_TERRAFORM_PATH
// names (`make testacc TF_BIN=tofu`). The two differ in where `k0rdent/k0rdent`
// resolves and in a few features only Terraform has. This file is the one
// place that knows which CLI is under test.

// openTofuRegistry is where OpenTofu resolves a source address with no host.
const openTofuRegistry = "registry.opentofu.org"

// cliIsOpenTofu reads the first line of `<cli> version`, which names the
// product. `version -json` doesn't: both report a "terraform_version", and
// OpenTofu's is its own version number, so comparing it with a Terraform
// release says nothing.
var cliIsOpenTofu = sync.OnceValues(func() (bool, error) {
	bin := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if bin == "" {
		found, err := exec.LookPath("terraform")
		if err != nil {
			return false, fmt.Errorf("TF_ACC_TERRAFORM_PATH is unset and no terraform is on PATH: %w", err)
		}
		bin = found
	}
	cmd := exec.Command(bin, "version") //nolint:gosec // the CLI under test
	cmd.Env = append(quietEnviron(), "CHECKPOINT_DISABLE=1")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("%s version: %w", bin, err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	switch {
	case strings.HasPrefix(first, "OpenTofu "):
		return true, nil
	case strings.HasPrefix(first, "Terraform "):
		return false, nil
	}
	return false, fmt.Errorf("%s version printed %q, which names neither Terraform nor OpenTofu", bin, first)
})

// underOpenTofu is whether the CLI under test is OpenTofu. A CLI that can't
// say what it is fails the test under TF_ACC rather than being guessed at.
// Without TF_ACC there's no CLI to need, as in terraformForMigration.
func underOpenTofu(t *testing.T) bool {
	t.Helper()
	tofu, err := cliIsOpenTofu()
	if err != nil {
		if os.Getenv("TF_ACC") == "" {
			skipTest(t, err.Error())
		}
		t.Fatal(err)
	}
	return tofu
}

// skipUnderOpenTofu is the one way a test leaves OpenTofu out. why is one of
// the reasons below, and TESTING.md lists every one.
func skipUnderOpenTofu(t *testing.T, why string) {
	t.Helper()
	if underOpenTofu(t) {
		skipTest(t, "not under OpenTofu: "+why)
	}
}

// taintedReplaced is how the CLI under test says a plan replaces a tainted
// resource. OpenTofu adds an "it".
func taintedReplaced(t *testing.T) string {
	t.Helper()
	if underOpenTofu(t) {
		return "is tainted, so it must be replaced"
	}
	return "is tainted, so must be replaced"
}

// openTofuUnquotedMapKey is the error `tofu plan -generate-config-out` gives
// for a map attribute keyed by something that isn't an HCL identifier, such
// as `10.0.0.31`. OpenTofu 1.12 and 1.13 write each key of a nested map
// attribute with "%s = {" in internal/genconfig/generate_config.go, where
// Terraform quotes it, then fail to parse what they wrote.
var openTofuUnquotedMapKey = regexp.MustCompile(`(?s)generating plan with import config.*Invalid number literal.*on generated\.tf`)

// Why a test is left out under OpenTofu. Each is something OpenTofu lacks or
// does differently, seen on OpenTofu 1.12 and 1.13, and none is the
// provider's.
const (
	// Action blocks, action_trigger and `apply -invoke` are Terraform 1.14
	// features. `tofu apply -invoke` fails with "flag provided but not
	// defined".
	openTofuHasNoActions = "OpenTofu has no actions (action blocks, action_trigger, apply -invoke)"

	// `terraform query` and list resources are Terraform 1.14 features.
	openTofuHasNoQuery = "OpenTofu has no query command or list resources"

	// The test records that Terraform opens an ephemeral resource at plan
	// even under depends_on. OpenTofu defers the open, so the error the test
	// expects never comes.
	openTofuDefersTheOpen = "OpenTofu defers an ephemeral open under depends_on, and the test records that Terraform doesn't"

	// The test checks the kubeconfig in a postcondition during the apply
	// that creates the cluster. OpenTofu evaluates an ephemeral resource's
	// postconditions at plan while its open is deferred, with cluster_id
	// null, and doesn't open an ephemeral resource nothing refers to during
	// that apply.
	openTofuChecksAnUnopenedEphemeral = "OpenTofu evaluates an unopened ephemeral resource's postconditions at plan, with its arguments null"
)

// providerAddress is the address `k0rdent/k0rdent` resolves to under the CLI
// under test: the address its dev_overrides and its state files name.
func providerAddress(t *testing.T) string {
	t.Helper()
	if underOpenTofu(t) {
		return openTofuRegistry + "/" + provider.RegistryNamespace + "/" + provider.ProviderTypeName
	}
	return provider.RegistryAddress()
}

// devOverridesRC is a CLI configuration that runs the build in dir in place
// of the registry's provider.
func devOverridesRC(t *testing.T, dir string) string {
	t.Helper()
	return fmt.Sprintf("provider_installation {\n  dev_overrides {\n    %q = %q\n  }\n  direct {}\n}\n",
		providerAddress(t), dir)
}
