package testmatrix

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// bindingServer answers like a resource with no id attribute, such as the
// VPC security groups binding: its objects are told apart by vpc, project and
// region. secret_wo is write-only. Only the calls the recorder makes in these
// tests are implemented.
type bindingServer struct {
	server
}

const bindingType = "test_binding"

var bindingSchema = &tfprotov6.Schema{Block: &tfprotov6.SchemaBlock{Attributes: []*tfprotov6.SchemaAttribute{
	{Name: "vpc", Type: tftypes.String, Required: true},
	{Name: "project", Type: tftypes.String, Optional: true, Computed: true},
	{Name: "region", Type: tftypes.String, Optional: true, Computed: true},
	{Name: "security_groups", Type: tftypes.Set{ElementType: tftypes.String}, Required: true},
	{Name: "secret_wo", Type: tftypes.String, Optional: true, WriteOnly: true},
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

// ImportResourceState imports the binding on the VPC the request's id names.
func (bindingServer) ImportResourceState(_ context.Context, req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	return &tfprotov6.ImportResourceStateResponse{ImportedResources: []*tfprotov6.ImportedResource{{TypeName: req.TypeName, State: binding(req.ID, "")}}}, nil
}

// ReadResource finds the binding as it was.
func (bindingServer) ReadResource(_ context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	return &tfprotov6.ReadResourceResponse{NewState: req.CurrentState}, nil
}

// PlanResourceChange plans no change.
func (bindingServer) PlanResourceChange(_ context.Context, req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	return &tfprotov6.PlanResourceChangeResponse{PlannedState: req.PriorState}, nil
}

// binding is the binding on vpc, with secret as its write-only value when
// it's a configuration. A state's is always null.
func binding(vpc, secret string) *tfprotov6.DynamicValue {
	typ := bindingSchema.ValueType()
	wo := tftypes.NewValue(tftypes.String, nil)
	if secret != "" {
		wo = tftypes.NewValue(tftypes.String, secret)
	}
	dv, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, map[string]tftypes.Value{
		"vpc":     tftypes.NewValue(tftypes.String, vpc),
		"project": tftypes.NewValue(tftypes.String, "p"),
		"region":  tftypes.NewValue(tftypes.String, "r"),
		"security_groups": tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, []tftypes.Value{
			tftypes.NewValue(tftypes.String, "default"),
		}),
		"secret_wo": wo,
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
	s, err := Wrap(bindingServer{}, file, "TestBindings")
	if err != nil {
		t.Fatal(err)
	}
	return command{t, s}
}

func (p command) importBinding(vpc string) {
	p.t.Helper()
	if _, err := p.s.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: bindingType, ID: vpc}); err != nil {
		p.t.Fatal(err)
	}
}

// read reads the binding on vpc back.
func (p command) read(vpc string) {
	p.t.Helper()
	if _, err := p.s.ReadResource(context.Background(), &tfprotov6.ReadResourceRequest{TypeName: bindingType, CurrentState: binding(vpc, "")}); err != nil {
		p.t.Fatal(err)
	}
}

// plan plans the binding on vpc, held in state, with secret in its
// configuration.
func (p command) plan(vpc, secret string) {
	p.t.Helper()
	if _, err := p.s.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName: bindingType, PriorState: binding(vpc, ""), ProposedNewState: binding(vpc, ""), Config: binding(vpc, secret),
	}); err != nil {
		p.t.Fatal(err)
	}
}

// recorded finishes the record and reads it.
func recorded(t *testing.T, file string) []Line {
	t.Helper()
	if err := Finish(file); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing recorded
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read only
	var lines []Line
	s := bufio.NewScanner(f)
	for s.Scan() {
		var l Line
		if err := json.Unmarshal(s.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	return lines
}

func cells(lines []Line) map[string]int {
	out := map[string]int{}
	for _, l := range lines {
		out[l.Cell+" "+l.Path]++
	}
	return out
}

// Terraform imports and plans the objects of one plan in parallel. Two
// bindings imported together, each call for the first followed by the same
// call for the second, both credit the test with their import cells. Keyed
// by id, which a binding hasn't got, they shared one memory and the order of
// the calls decided whether either did.
func TestTwoObjectsWithNoIDImportedTogetherAreBothCredited(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	p := start(t, file)
	p.importBinding("vpc-a")
	p.importBinding("vpc-b")
	p.read("vpc-a")
	p.read("vpc-b")
	p.plan("vpc-a", "")
	p.plan("vpc-b", "")
	got := cells(recorded(t, file))
	for _, path := range []string{"vpc", "project", "region", "security_groups"} {
		if got["import "+path] == 0 {
			t.Errorf("import %s: not recorded", path)
		}
	}
	// A binding with no id is still one object of many: an update of one
	// isn't measured against another's configuration.
	file = filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).plan("vpc-a", "one")
	next := start(t, file)
	next.plan("vpc-b", "two")
	next.plan("vpc-a", "one")
	if got := recorded(t, file); len(got) != 0 {
		t.Errorf("two bindings planned with their own unchanged configurations recorded %v", got)
	}
}

// terraform-plugin-testing's generate step plans the same object twice in
// one command: once from state with the test's configuration, and once
// imported, with the generated configuration, which can't hold a write-only
// value. The record is the same whatever order those calls come in, and the
// one from state, whose configuration didn't change, is no update.
func TestTheSameObjectPlannedTwiceInOneCommandRecordsTheSameInAnyOrder(t *testing.T) {
	type call func(command)
	fromState := func(p command) { p.read("vpc-a"); p.plan("vpc-a", "s3cret") }
	imported := func(p command) { p.importBinding("vpc-a"); p.read("vpc-a"); p.plan("vpc-a", "") }
	orders := map[string][]call{
		"state first":  {fromState, imported},
		"import first": {imported, fromState},
	}
	var want []Line
	for name, order := range orders {
		file := filepath.Join(t.TempDir(), "record.jsonl")
		start(t, file).plan("vpc-a", "s3cret") // the step before
		p := start(t, file)
		for _, c := range order {
			c(p)
		}
		got := recorded(t, file)
		if cells(got)["update secret_wo"] != 0 {
			t.Errorf("%s: recorded an update of secret_wo, which no configuration changed", name)
		}
		if want == nil {
			want = got
		} else if !slices.Equal(got, want) {
			t.Errorf("%s: recorded\n%v\nwant what the other order recorded\n%v", name, got, want)
		}
	}
	if cells(want)["import vpc"] != 1 {
		t.Errorf("the import's plan wasn't credited: %v", want)
	}
}

// A configuration changed from the last command's counts, in every call of
// the command that plans it.
func TestAChangedWriteOnlyValueIsAnUpdate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	start(t, file).plan("vpc-a", "one")
	next := start(t, file)
	next.plan("vpc-a", "two")
	next.plan("vpc-a", "two") // the apply's own plan, in the same command
	got := cells(recorded(t, file))
	if got["update secret_wo"] != 1 || len(got) != 1 {
		t.Errorf("recorded %v, want the one update of secret_wo", got)
	}
}

// A resource with no identity schema can't be told apart from its other
// objects, so the recorder refuses it rather than mixing them up.
func TestAResourceWithNoIdentityFailsTheRecord(t *testing.T) {
	file := filepath.Join(t.TempDir(), "record.jsonl")
	s, err := Wrap(noIdentityServer{}, file, "TestBindings")
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

// `terraform import`, then a plan with -refresh=false: the plan is the first
// thing after the import and its read, in the next command. A plan that
// reads the object again first is a plan of what it read.
func TestAPlanInTheCommandAfterAnImportFollowsIt(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		file := filepath.Join(t.TempDir(), "record.jsonl")
		imp := start(t, file)
		imp.importBinding("vpc-a")
		imp.read("vpc-a")
		plan := start(t, file)
		if refresh {
			plan.read("vpc-a")
		}
		plan.plan("vpc-a", "")
		got := cells(recorded(t, file))["import vpc"]
		if refresh && got != 0 {
			t.Error("a plan that read the object again was credited with the import")
		}
		if !refresh && got == 0 {
			t.Error("a plan with -refresh=false straight after `terraform import` wasn't credited with it")
		}
	}
}
