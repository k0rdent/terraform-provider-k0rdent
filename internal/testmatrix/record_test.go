package testmatrix

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// bindingServer answers like a resource with no id attribute, such as the
// VPC security groups binding: its objects are told apart by vpc, project and
// region. secret_wo is write-only, and label changes in place. With replace, a
// change of security_groups replaces it. A plan to attach "bad" fails.
type bindingServer struct {
	server
	replace bool
}

const bindingType = "test_binding"

var bindingSchema = &tfprotov6.Schema{Block: &tfprotov6.SchemaBlock{Attributes: []*tfprotov6.SchemaAttribute{
	{Name: "vpc", Type: tftypes.String, Required: true},
	{Name: "project", Type: tftypes.String, Optional: true, Computed: true},
	{Name: "region", Type: tftypes.String, Optional: true, Computed: true},
	{Name: "security_groups", Type: tftypes.Set{ElementType: tftypes.String}, Required: true},
	{Name: "secret_wo", Type: tftypes.String, Optional: true, WriteOnly: true},
	{Name: "label", Type: tftypes.String, Optional: true},
}}}

func (bindingServer) GetProviderSchema(context.Context, *tfprotov6.GetProviderSchemaRequest) (*tfprotov6.GetProviderSchemaResponse, error) {
	return &tfprotov6.GetProviderSchemaResponse{ResourceSchemas: map[string]*tfprotov6.Schema{bindingType: bindingSchema}}, nil
}

func (bindingServer) GetResourceIdentitySchemas(context.Context, *tfprotov6.GetResourceIdentitySchemasRequest) (*tfprotov6.GetResourceIdentitySchemasResponse, error) {
	return &tfprotov6.GetResourceIdentitySchemasResponse{IdentitySchemas: map[string]*tfprotov6.ResourceIdentitySchema{
		bindingType: {IdentityAttributes: []*tfprotov6.ResourceIdentitySchemaAttribute{
			{Name: "vpc", Type: tftypes.String, RequiredForImport: true},
			{Name: "project", Type: tftypes.String, OptionalForImport: true},
			{Name: "region", Type: tftypes.String, OptionalForImport: true},
		}},
	}}, nil
}

func (bindingServer) ImportResourceState(_ context.Context, req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	return &tfprotov6.ImportResourceStateResponse{ImportedResources: []*tfprotov6.ImportedResource{{TypeName: req.TypeName, State: binding(req.ID, "", "default")}}}, nil
}

func (bindingServer) ReadResource(_ context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	return &tfprotov6.ReadResourceResponse{NewState: req.CurrentState}, nil
}

func (b bindingServer) PlanResourceChange(_ context.Context, req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	resp := &tfprotov6.PlanResourceChangeResponse{PlannedState: req.ProposedNewState}
	if b.replace {
		resp.RequiresReplace = []*tftypes.AttributePath{tftypes.NewAttributePath().WithAttributeName("security_groups")}
	}
	if bytes.Equal(req.ProposedNewState.MsgPack, binding("vpc-a", "", "bad").MsgPack) {
		resp.Diagnostics = []*tfprotov6.Diagnostic{{Severity: tfprotov6.DiagnosticSeverityError, Summary: "bad"}}
	}
	return resp, nil
}

func (bindingServer) ApplyResourceChange(_ context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	return &tfprotov6.ApplyResourceChangeResponse{NewState: req.PlannedState}, nil
}

// binding is the binding on vpc with group attached, and secret as its
// write-only value when it's a configuration.
func binding(vpc, secret, group string) *tfprotov6.DynamicValue {
	return labelled(vpc, secret, group, "")
}

// labelled is binding with label set, when it isn't empty.
func labelled(vpc, secret, group, label string) *tfprotov6.DynamicValue {
	typ := bindingSchema.ValueType()
	str := func(s string) tftypes.Value {
		if s == "" {
			return tftypes.NewValue(tftypes.String, nil)
		}
		return tftypes.NewValue(tftypes.String, s)
	}
	dv, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, map[string]tftypes.Value{
		"vpc":     tftypes.NewValue(tftypes.String, vpc),
		"project": tftypes.NewValue(tftypes.String, "p"),
		"region":  tftypes.NewValue(tftypes.String, "r"),
		"security_groups": tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, []tftypes.Value{
			tftypes.NewValue(tftypes.String, group),
		}),
		"secret_wo": str(secret),
		"label":     str(label),
	}))
	if err != nil {
		panic(err)
	}
	return &dv
}

// command is one recorder around bindingServer: one Terraform command.
type command struct {
	t *testing.T
	s tfprotov6.ProviderServer
}

func start(t *testing.T, file string) command {
	t.Helper()
	return startWith(t, file, bindingServer{})
}

func startWith(t *testing.T, file string, b bindingServer) command {
	t.Helper()
	s, err := Wrap(b, file, "TestBindings", "")
	if err != nil {
		t.Fatal(err)
	}
	return command{t, s}
}

func (c command) importBinding(vpc string) {
	c.t.Helper()
	if _, err := c.s.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: bindingType, ID: vpc}); err != nil {
		c.t.Fatal(err)
	}
}

func (c command) read(vpc string) {
	c.t.Helper()
	if _, err := c.s.ReadResource(context.Background(), &tfprotov6.ReadResourceRequest{TypeName: bindingType, CurrentState: binding(vpc, "", "default")}); err != nil {
		c.t.Fatal(err)
	}
}

// plan plans the binding on vpc, attached to default, with secret in its
// configuration and nothing to change.
func (c command) plan(vpc, secret string) {
	c.t.Helper()
	c.planGroup(vpc, secret, "default", "default")
}

// planGroup plans it with group in its configuration and planned attached.
func (c command) planGroup(vpc, secret, group, planned string) {
	c.t.Helper()
	if _, err := c.s.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName:         bindingType,
		PriorState:       binding(vpc, "", "default"),
		ProposedNewState: binding(vpc, "", planned),
		Config:           binding(vpc, secret, group),
	}); err != nil {
		c.t.Fatal(err)
	}
}

// planValues plans the binding with these prior, planned and configured
// values.
func (c command) planValues(prior, planned, config *tfprotov6.DynamicValue) {
	c.t.Helper()
	if _, err := c.s.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName:         bindingType,
		PriorState:       prior,
		ProposedNewState: planned,
		Config:           config,
	}); err != nil {
		c.t.Fatal(err)
	}
}

// apply applies a binding on vpc, attached to group, with secret.
func (c command) apply(vpc, secret, group string) {
	c.t.Helper()
	if _, err := c.s.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:     bindingType,
		PlannedState: binding(vpc, "", group),
		Config:       binding(vpc, secret, group),
	}); err != nil {
		c.t.Fatal(err)
	}
}

// recorded finishes the record and gives its cells, as "cell path", sorted.
func recorded(t *testing.T, file string) []string {
	t.Helper()
	if err := Finish(file, true); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read only
	var cells []string
	finished := false
	s := bufio.NewScanner(f)
	for s.Scan() {
		var l Line
		if err := json.Unmarshal(s.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		if l.Cell == CellFinished {
			finished = true
			continue
		}
		cells = append(cells, l.Cell+" "+l.Path)
	}
	if !finished {
		t.Error("the record has no finished line")
	}
	slices.Sort(cells)
	return cells
}

func expect(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		t.Errorf("%s: recorded\n  %s\nwant\n  %s", name, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

var importedBinding = []string{"import project", "import region", "import security_groups", "import vpc"}

// Two bindings imported in one plan, their calls interleaved, are two
// objects, each credited with its import.
func TestTwoObjectsWithNoIDImportedTogetherAreBothCredited(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	c := start(t, file)
	c.importBinding("vpc-a")
	c.importBinding("vpc-b")
	c.read("vpc-a")
	c.read("vpc-b")
	c.plan("vpc-a", "")
	c.plan("vpc-b", "")
	expect(t, "import", recorded(t, file), importedBinding...)

	// Nor is one binding's configuration measured against another's.
	file = filepath.Join(t.TempDir(), "record.jsonl")
	c = start(t, file)
	c.apply("vpc-a", "one", "default")
	c.apply("vpc-b", "two", "default")
	c = start(t, file)
	c.read("vpc-b")
	c.plan("vpc-b", "two")
	c.read("vpc-a")
	c.plan("vpc-a", "one")
	expect(t, "unchanged", recorded(t, file))
}

// terraform-plugin-testing's generate step plans the object twice in one
// command: the copy in state, with the test's configuration, and the
// import, with the generated one, which can't hold a write-only value. In
// either order, only what the import's plan keeps is an import, and the
// copy, whose configuration didn't change, is no update.
func TestTheSameObjectPlannedTwiceInOneCommandRecordsTheSameInAnyOrder(t *testing.T) {
	fromState := func(c command) { c.read("vpc-a"); c.plan("vpc-a", "s3cret") }
	imported := func(c command) { c.importBinding("vpc-a"); c.read("vpc-a"); c.plan("vpc-a", "") }
	for name, order := range map[string][]func(command){
		"state first":  {fromState, imported},
		"import first": {imported, fromState},
	} {
		file := filepath.Join(t.TempDir(), "record.jsonl")
		start(t, file).apply("vpc-a", "s3cret", "default")
		c := start(t, file)
		for _, call := range order {
			call(c)
		}
		expect(t, name, recorded(t, file), importedBinding...)
	}
}

// The query test applies a subscription with a secret in one working
// directory, adopts it in another with generated configuration, which has
// none, then destroys it from the first, whose refresh plans it with the
// secret again. That plan changed nothing of its own configuration.
func TestAdoptingAnObjectIsNoChangeToTheConfigurationThatMadeIt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "s3cret", "default")
	adopt := start(t, file)
	adopt.importBinding("vpc-a")
	adopt.read("vpc-a")
	adopt.plan("vpc-a", "")
	destroy := start(t, file)
	destroy.read("vpc-a")
	destroy.plan("vpc-a", "s3cret")
	expect(t, "adopted", recorded(t, file), importedBinding...)
}

// Two working directories that both hold the object each keep their own
// configuration: a plan in one is measured against that one's last. Only a
// built binary runs in its workspace, as t.Chdir has these do.
func TestEachWorkingDirectoryKeepsItsOwnConfiguration(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	a, b := t.TempDir(), t.TempDir()
	t.Chdir(a)
	start(t, file).apply("vpc-a", "one", "default")
	t.Chdir(b)
	other := start(t, file)
	other.read("vpc-a")
	other.plan("vpc-a", "two")
	t.Chdir(a)
	again := start(t, file)
	again.read("vpc-a")
	again.plan("vpc-a", "one")
	expect(t, "two directories", recorded(t, file))
}

// An import in another working directory isn't this one's: a plan here
// with -refresh=false doesn't follow it.
func TestAnImportInAnotherDirectoryIsNotThisOnes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	a, b := t.TempDir(), t.TempDir()
	t.Chdir(a)
	start(t, file).apply("vpc-a", "", "default")
	t.Chdir(b)
	imp := start(t, file)
	imp.importBinding("vpc-a")
	imp.read("vpc-a")
	t.Chdir(a)
	start(t, file).plan("vpc-a", "")
	expect(t, "other directory", recorded(t, file))
}

// In process every provider runs in the test's directory, so a workspace
// named on Wrap stands for the working directory it can't see: an import
// there isn't followed by a plan in the test's other steps'.
func TestANamedWorkspaceKeepsAnImportToItself(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "", "default")
	s, err := Wrap(bindingServer{}, file, "TestBindings", "import step 1")
	if err != nil {
		t.Fatal(err)
	}
	imp := command{t, s}
	imp.importBinding("vpc-a")
	imp.read("vpc-a")
	start(t, file).plan("vpc-a", "")
	expect(t, "import step", recorded(t, file))
}

// A plan that moves an imported value doesn't keep it, so it isn't credited
// with importing it.
func TestAnImportedValueThePlanMovesIsNoImport(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	c := start(t, file)
	c.importBinding("vpc-a")
	c.read("vpc-a")
	c.planGroup("vpc-a", "", "web", "web")
	expect(t, "moved", recorded(t, file), "import project", "import region", "import vpc")
}

// A plan after an import is an import, never an update, even when its
// configuration differs from the one before.
func TestAPlanAfterAnImportIsNoUpdate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "one", "default")
	c := start(t, file)
	c.importBinding("vpc-a")
	c.read("vpc-a")
	c.plan("vpc-a", "two")
	expect(t, "imported", recorded(t, file), append([]string{"import secret_wo"}, importedBinding...)...)
}

// A change the RequiresReplace paths cover is a replace.
func TestAChangeThatReplacesIsAReplace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "", "default")
	startWith(t, file, bindingServer{replace: true}).planGroup("vpc-a", "", "web", "web")
	expect(t, "replace", recorded(t, file), "replace security_groups")
}

// A plan that fails, as an ExpectError step's does, credits nothing.
func TestAPlanThatFailsCreditsNothing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "", "default")
	start(t, file).planGroup("vpc-a", "", "bad", "bad")
	expect(t, "failed", recorded(t, file))
}

// A plan is measured against the last command's configuration, not every
// earlier one: changing a value back is a change.
func TestChangingAValueBackIsAnUpdate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "", "default")
	start(t, file).planGroup("vpc-a", "", "web", "default") // a change the plan doesn't move
	start(t, file).planGroup("vpc-a", "", "default", "web")
	expect(t, "back", recorded(t, file), "update security_groups")
}

// Only a whole run that passed ends the record with the line the check needs.
func TestOnlyACompleteRunIsFinished(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "", "default")
	if err := Finish(file, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(b), CellFinished) {
		t.Errorf("an incomplete run's record is finished:\n%s", b)
	}
}

// A test that skipped left its cells out, so the run isn't the whole
// suite's, even though it passed: the record names the test and isn't
// finished.
func TestARunWhereATestSkippedIsNotFinished(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "", "default")
	Skipped(file, "TestQuery", "terraform 1.13.0 has no `terraform query`")
	Skipped(file, "TestQuery", "terraform 1.13.0 has no `terraform query`")
	if err := Finish(file, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"test":"TestQuery","resource":"","path":"","cell":"skipped","why":"terraform 1.13.0 has no ` + "`terraform query`" + `"}` + "\n"
	if string(b) != want {
		t.Errorf("recorded\n%s\nwant\n%s", b, want)
	}
}

// Every plan of one command is measured against the commands before it,
// never against another plan of the same command, so their order doesn't
// matter.
func TestEveryPlanOfOneCommandIsMeasuredAgainstTheOnesBefore(t *testing.T) {
	still := func(c command) { c.planGroup("vpc-a", "", "web", "default") }
	moves := func(c command) { c.planGroup("vpc-a", "", "web", "web") }
	for name, order := range map[string][]func(command){
		"still first": {still, moves},
		"moves first": {moves, still},
	} {
		file := filepath.Join(t.TempDir(), "record.jsonl")
		start(t, file).apply("vpc-a", "", "default")
		c := start(t, file)
		for _, call := range order {
			call(c)
		}
		expect(t, name, recorded(t, file), "update security_groups")
	}
}

// A write-only value is never in state, so a change to it alone plans no
// change, Terraform applies nothing, and the provider never gets it: no
// update. Beside a change Terraform applies, it's an update, once.
func TestAWriteOnlyValueChangedAloneIsNoUpdate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "one", "default")
	c := start(t, file)
	c.plan("vpc-a", "two")
	c.plan("vpc-a", "two")
	expect(t, "alone", recorded(t, file))

	file = filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "one", "default")
	c = start(t, file)
	c.planValues(binding("vpc-a", "", "default"), labelled("vpc-a", "", "default", "web"), labelled("vpc-a", "two", "default", "web"))
	c.planValues(binding("vpc-a", "", "default"), labelled("vpc-a", "", "default", "web"), labelled("vpc-a", "two", "default", "web"))
	expect(t, "beside an update", recorded(t, file), "update label", "update secret_wo")
}

// A plan that replaces the object updates nothing in place: what else it
// changes, write-only values included, is part of the new object.
func TestAReplacingPlanUpdatesNothingInPlace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).apply("vpc-a", "one", "default")
	startWith(t, file, bindingServer{replace: true}).planValues(
		binding("vpc-a", "", "default"), labelled("vpc-a", "", "web", "new"), labelled("vpc-a", "two", "web", "new"))
	expect(t, "replace", recorded(t, file), "replace security_groups")
}

// A plan after an import that replaces the object keeps nothing it
// imported, not even the write-only value its configuration sets.
func TestAReplacingPlanAfterAnImportIsNoImport(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	c := startWith(t, file, bindingServer{replace: true})
	c.importBinding("vpc-a")
	c.read("vpc-a")
	c.planGroup("vpc-a", "s3cret", "web", "web")
	expect(t, "replaced", recorded(t, file))
}

// `terraform import`, then a plan with -refresh=false: the plan is the first
// thing after the import, in the next command. A plan that reads the object
// again first is a plan of what it read.
func TestAPlanInTheCommandAfterAnImportFollowsIt(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		file := filepath.Join(t.TempDir(), "record.jsonl")
		imp := start(t, file)
		imp.importBinding("vpc-a")
		imp.read("vpc-a")
		plan := start(t, file)
		if refresh {
			plan.read("vpc-a")
			plan.plan("vpc-a", "")
			expect(t, "refreshed", recorded(t, file))
		} else {
			plan.plan("vpc-a", "")
			expect(t, "not refreshed", recorded(t, file), importedBinding...)
		}
	}
}

// A resource with no identity schema can't be told apart from its other
// objects, so the recorder refuses it rather than mixing them up.
func TestAResourceWithNoIdentityFailsTheRecord(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	s, err := Wrap(noIdentityServer{}, file, "TestBindings", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: bindingType, ID: "vpc-a"})
	if err == nil {
		t.Fatal("an import of a resource with no identity schema recorded without an error")
	}
}

type noIdentityServer struct {
	bindingServer
}

func (noIdentityServer) GetResourceIdentitySchemas(context.Context, *tfprotov6.GetResourceIdentitySchemasRequest) (*tfprotov6.GetResourceIdentitySchemasResponse, error) {
	return &tfprotov6.GetResourceIdentitySchemasResponse{}, nil
}

// versionedServer is bindingServer at schema version 1.
type versionedServer struct {
	bindingServer
}

func (versionedServer) GetProviderSchema(context.Context, *tfprotov6.GetProviderSchemaRequest) (*tfprotov6.GetProviderSchemaResponse, error) {
	s := *bindingSchema
	s.Version = 1
	return &tfprotov6.GetProviderSchemaResponse{ResourceSchemas: map[string]*tfprotov6.Schema{bindingType: &s}}, nil
}

func (versionedServer) UpgradeResourceState(context.Context, *tfprotov6.UpgradeResourceStateRequest) (*tfprotov6.UpgradeResourceStateResponse, error) {
	return &tfprotov6.UpgradeResourceStateResponse{UpgradedState: binding("vpc-a", "", "default")}, nil
}

// Terraform asks the provider to upgrade every state it reads, at the
// current version too. Only a state from an older version is an upgrade.
func TestOnlyAStateFromAnOlderVersionIsAnUpgrade(t *testing.T) {
	for version, want := range map[int64][]string{
		0: {"upgrade project", "upgrade region", "upgrade security_groups", "upgrade vpc"},
		1: nil,
	} {
		file := filepath.Join(t.TempDir(), "record.jsonl")
		s, err := Wrap(versionedServer{}, file, "TestBindings", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpgradeResourceState(context.Background(), &tfprotov6.UpgradeResourceStateRequest{TypeName: bindingType, Version: version}); err != nil {
			t.Fatal(err)
		}
		expect(t, fmt.Sprintf("from version %d", version), recorded(t, file), want...)
	}
}
