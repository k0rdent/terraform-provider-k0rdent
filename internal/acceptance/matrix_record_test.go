package acceptance

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
	"github.com/k0rdent/terraform-provider-k0rdent/internal/testmatrix"
)

// The testing matrix's recorder. `make testacc-matrix` passes -matrix-record,
// and every provider server a test starts, in process through protoV6 or as a
// built binary under the CLI, records the matrix cells the test exercised.
// See internal/testmatrix and docs/testing-matrix.md.
var matrixRecord = flag.String("matrix-record", "",
	"append the testing matrix cells each test exercises to this file")

// startMatrixRecord starts a fresh record when -matrix-record names one. The
// binaries this run starts inherit the file through the environment, and each
// is told its test by matrixTestEnv.
func startMatrixRecord() error {
	if *matrixRecord == "" {
		return nil
	}
	file, err := filepath.Abs(*matrixRecord)
	if err != nil {
		return err
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		return fmt.Errorf("starting the matrix record: %w", err)
	}
	if err := os.RemoveAll(file + ".memory"); err != nil {
		return fmt.Errorf("clearing the last run's matrix memory: %w", err)
	}
	if err := os.Setenv(testmatrix.RecordEnv, file); err != nil {
		return err
	}
	inner := protoV6[provider.ProviderTypeName]
	protoV6[provider.ProviderTypeName] = func() (tfprotov6.ProviderServer, error) {
		// terraform-plugin-testing calls the factory on the test's own
		// goroutine, so the test is on the stack.
		test := callingTest()
		s, err := inner()
		if err != nil {
			return s, err
		}
		return testmatrix.Wrap(s, file, test)
	}
	return nil
}

// matrixTestEnv is what a built provider binary needs in its environment to
// record under t's test. Every command that runs builtProvider's binary
// appends it. A binary started without it records under "unknown", and the
// matrix check refuses that.
func matrixTestEnv(t *testing.T) []string {
	if os.Getenv(testmatrix.RecordEnv) == "" {
		return nil
	}
	name, _, _ := strings.Cut(t.Name(), "/")
	return []string{testmatrix.TestEnv + "=" + name}
}

// callingTest is the top-level Test function on the stack.
func callingTest() string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if _, name, ok := strings.Cut(f.Function, "internal/acceptance.Test"); ok {
			name, _, _ = strings.Cut(name, ".")
			return "Test" + name
		}
		if !more {
			return testmatrix.UnknownTest
		}
	}
}
