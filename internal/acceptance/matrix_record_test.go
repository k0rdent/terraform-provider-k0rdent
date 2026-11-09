package acceptance

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
	"github.com/k0rdent/terraform-provider-k0rdent/internal/testmatrix"
)

// The testing matrix's recorder. `make testacc` passes -matrix-record,
// and every provider server a test starts, in process through protoV6 or as a
// built binary under the CLI, records the matrix cells the test exercised.
// See internal/testmatrix and docs/testing-matrix.md.
var matrixRecord = flag.String("matrix-record", "",
	"append the testing matrix cells each test exercises to this file")

// matrixFile is -matrix-record's absolute path, once startMatrixRecord has
// started the record.
var matrixFile string

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
	matrixFile = file
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

// skipTest skips t, and while the run records the testing matrix notes the
// skip first, so the record of a run where a test skipped isn't taken for the
// whole suite's. It's the one way a test here skips
// (TestEveryAcceptanceSkipIsRecorded).
func skipTest(t *testing.T, why string) {
	t.Helper()
	if matrixFile != "" {
		name, _, _ := strings.Cut(t.Name(), "/")
		testmatrix.Skipped(matrixFile, name, why)
	}
	t.Skip(why)
}

// recordedVersionCheck is a Terraform version check that notes a skip the
// way skipTest does. terraform-plugin-testing runs the checks on the test's
// goroutine, so the test is on the stack.
type recordedVersionCheck struct {
	tfversion.TerraformVersionCheck
}

func (c recordedVersionCheck) CheckTerraformVersion(ctx context.Context, req tfversion.CheckTerraformVersionRequest, resp *tfversion.CheckTerraformVersionResponse) {
	c.TerraformVersionCheck.CheckTerraformVersion(ctx, req, resp)
	if resp.Skip != "" && matrixFile != "" {
		testmatrix.Skipped(matrixFile, callingTest(), resp.Skip)
	}
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
	t.Parallel()
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

// A test here skips only through skipTest, or a version check through
// recordedVersionCheck, so a run where one skipped never passes for the
// whole suite in the testing matrix's record.
func TestEveryAcceptanceSkipIsRecorded(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var allowed []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Name.Name == "skipTest" {
					allowed = append(allowed, n)
				}
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok && id.Name == "recordedVersionCheck" {
					allowed = append(allowed, n)
				}
			}
			return true
		})
		inside := func(n ast.Node) bool {
			return slices.ContainsFunc(allowed, func(a ast.Node) bool { return a.Pos() <= n.Pos() && n.End() <= a.End() })
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || inside(sel) {
				return true
			}
			x, isIdent := sel.X.(*ast.Ident)
			switch {
			case slices.Contains([]string{"Skip", "Skipf", "SkipNow"}, sel.Sel.Name):
				t.Errorf("%s: %s skips without noting it in the testing matrix's record: use skipTest", fset.Position(sel.Pos()), sel.Sel.Name)
			case isIdent && x.Name == "tfversion" && strings.HasPrefix(sel.Sel.Name, "Skip"):
				t.Errorf("%s: tfversion.%s skips without noting it in the testing matrix's record: wrap it in recordedVersionCheck",
					fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
}
