package acceptance

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
	"github.com/k0rdent/terraform-provider-k0rdent/internal/testmatrix"
)

// The testing matrix's recorder. `make testacc` passes -matrix-record,
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
	if err := os.RemoveAll(file + ".facts"); err != nil {
		return fmt.Errorf("clearing the last run's matrix facts: %w", err)
	}
	if err := os.Setenv(testmatrix.RecordEnv, file); err != nil {
		return err
	}
	protoV6[provider.ProviderTypeName] = recording(file, protoV6[provider.ProviderTypeName])
	return nil
}

// recording is factory with the recorder around each server it makes,
// recording to file.
func recording(file string, factory func() (tfprotov6.ProviderServer, error)) func() (tfprotov6.ProviderServer, error) {
	return func() (tfprotov6.ProviderServer, error) {
		// terraform-plugin-testing calls the factory on the test's own
		// goroutine, so the test and its step are on the stack.
		test, workspace := callingTest(), importStepWorkspace()
		s, err := factory()
		if err != nil {
			return s, err
		}
		return testmatrix.Wrap(s, file, test, workspace)
	}
}

// importSteps numbers the providers import steps start.
var importSteps atomic.Int64

// importStepWorkspace names a working directory of its own for a provider an
// import step starts, and is empty for any other. terraform-plugin-testing
// runs an import step in a new directory (testing_new_import_state.go), but a
// provider served in process can't see it, and the recorder would take it to
// be the test's other steps' directory. Each provider gets its own name,
// since no command of an import step follows another's import: the one that
// plans imports too. With ImportStatePersist the step runs in the test's
// directory after all, and then a plan after it without a read is only not
// credited with the import.
func importStepWorkspace() string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "terraform-plugin-testing/helper/resource.testStepNewImportState") {
			return fmt.Sprintf("import step %d", importSteps.Add(1))
		}
		if !more {
			return ""
		}
	}
}

// finishMatrixRecord writes the record's import, update and replace lines
// from what every provider the run started saw. Only a run of the whole
// suite that passed marks the record finished, which the check needs. See
// testmatrix.Finish.
func finishMatrixRecord(code int) error {
	if *matrixRecord == "" {
		return nil
	}
	file, err := filepath.Abs(*matrixRecord)
	if err != nil {
		return err
	}
	whole := true
	for _, name := range []string{"test.run", "test.skip"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			whole = false
		}
	}
	if err := testmatrix.Finish(file, whole && code == 0); err != nil {
		return fmt.Errorf("finishing the matrix record: %w", err)
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

// An import step runs in a working directory of its own, so a plan in the
// test's directory after it, with no read in between, follows no import: a
// PlanOnly step's first plan doesn't refresh. The test records to a file of
// its own, through the same factory the suite's record uses.
func TestAccAPlanAfterAnImportStepFollowsNoImport(t *testing.T) {
	const id = "acc-matrix-import-step"
	file := filepath.Join(t.TempDir(), "record.jsonl")
	config := projectConfig(id, "Import step", "the matrix recorder's import step")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			provider.ProviderTypeName: recording(file, providerserver.NewProtocol6WithError(provider.New("acc")())),
		},
		CheckDestroy: checkGone(projectType, projectPath),
		Steps: []resource.TestStep{
			{Config: config},
			{
				Config:                  config,
				ResourceName:            projectAddr,
				ImportState:             true,
				ImportStateId:           id,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
			{Config: config, PlanOnly: true},
		},
	})
	if err := testmatrix.Finish(file, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var l testmatrix.Line
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		if l.Cell == testmatrix.CellImport {
			t.Errorf("the PlanOnly step after the import step is credited with importing %s", l.Path)
		}
	}
}
