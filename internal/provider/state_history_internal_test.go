package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// The guard against a state shape changing without a migration.
//
// Every resource's stored shape at every schema version it has ever declared is
// committed under testdata/state-schemas/<suffix>/v<N>.json. The snapshot for
// the current version has to match the schema exactly, and the older ones are
// never rewritten, because they are what is sitting in people's state files.
// So a change to the shape fails here until the resource's history gains a
// step, and the step is the only way to raise the schema version. An attribute
// that leaves the shape fails it too, until the step says whether its value is
// dropped or carried somewhere, because a rename written as a remove and an add
// otherwise passes everything and empties the attribute in every state.
var updateStateSchemas = flag.Bool("update-state-schemas", false,
	"record the state schema snapshot for a resource's current version when it doesn't exist yet")

const stateSchemaDir = "testdata/state-schemas"

type registeredResource struct {
	suffix   string
	typeName string
	resource resource.Resource
	schema   schema.Schema
}

func registeredResources(t *testing.T) []registeredResource {
	t.Helper()
	ctx := context.Background()
	var out []registeredResource
	for _, newResource := range New("test")().Resources(ctx) {
		r := newResource()
		var md resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		var sr resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &sr)
		if sr.Diagnostics.HasError() {
			t.Fatalf("%s: schema: %v", md.TypeName, sr.Diagnostics)
		}
		out = append(out, registeredResource{
			suffix:   strings.TrimPrefix(md.TypeName, ResourcePrefix()),
			typeName: md.TypeName,
			resource: r,
			schema:   sr.Schema,
		})
	}
	if len(out) == 0 {
		t.Fatal("no resource is registered, so this proves nothing")
	}
	return out
}

// renderStateType is the shape Terraform stores a resource's state in, and
// nothing else: descriptions, validators and plan modifiers can change freely
// because no stored state depends on them. It's the JSON type notation
// Terraform itself uses, indented so that a change reads as a diff.
func renderStateType(ctx context.Context, s schema.Schema) []byte {
	out, err := json.MarshalIndent(stateTypeNotation(s.Type().TerraformType(ctx)), "", "  ")
	if err != nil {
		panic(err) // only strings, slices and maps of them
	}
	return append(out, '\n')
}

// stateTypeNotation is a type in Terraform's JSON type notation. tftypes can
// write it, and deprecates that for use outside terraform-plugin-go, so this
// is the handful of cases a resource's state can hold.
func stateTypeNotation(typ tftypes.Type) any {
	switch tt := typ.(type) {
	case tftypes.List:
		return []any{"list", stateTypeNotation(tt.ElementType)}
	case tftypes.Set:
		return []any{"set", stateTypeNotation(tt.ElementType)}
	case tftypes.Map:
		return []any{"map", stateTypeNotation(tt.ElementType)}
	case tftypes.Object:
		attrs := make(map[string]any, len(tt.AttributeTypes))
		for name, attr := range tt.AttributeTypes {
			attrs[name] = stateTypeNotation(attr)
		}
		return []any{"object", attrs}
	case tftypes.Tuple:
		elems := make([]any, len(tt.ElementTypes))
		for i, el := range tt.ElementTypes {
			elems[i] = stateTypeNotation(el)
		}
		return []any{"tuple", elems}
	}
	switch {
	case typ.Is(tftypes.String):
		return "string"
	case typ.Is(tftypes.Number):
		return "number"
	case typ.Is(tftypes.Bool):
		return "bool"
	}
	return "dynamic"
}

// parseStateType reads back what renderStateType wrote, for the same reason
// stateTypeNotation writes it.
func parseStateType(raw json.RawMessage) (tftypes.Type, error) {
	var primitive string
	if err := json.Unmarshal(raw, &primitive); err == nil {
		switch primitive {
		case "string":
			return tftypes.String, nil
		case "number":
			return tftypes.Number, nil
		case "bool":
			return tftypes.Bool, nil
		case "dynamic":
			return tftypes.DynamicPseudoType, nil
		}
		return nil, fmt.Errorf("unknown type %q", primitive)
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) < 2 {
		return nil, fmt.Errorf("not a type: %s", raw)
	}
	var kind string
	if err := json.Unmarshal(parts[0], &kind); err != nil {
		return nil, fmt.Errorf("not a type: %s", raw)
	}
	switch kind {
	case "list", "set", "map":
		el, err := parseStateType(parts[1])
		if err != nil {
			return nil, err
		}
		switch kind {
		case "list":
			return tftypes.List{ElementType: el}, nil
		case "set":
			return tftypes.Set{ElementType: el}, nil
		}
		return tftypes.Map{ElementType: el}, nil
	case "object":
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(parts[1], &fields); err != nil {
			return nil, err
		}
		attrs := make(map[string]tftypes.Type, len(fields))
		for name, field := range fields {
			t, err := parseStateType(field)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			attrs[name] = t
		}
		return tftypes.Object{AttributeTypes: attrs}, nil
	case "tuple":
		var fields []json.RawMessage
		if err := json.Unmarshal(parts[1], &fields); err != nil {
			return nil, err
		}
		var elems []tftypes.Type
		for _, field := range fields {
			t, err := parseStateType(field)
			if err != nil {
				return nil, err
			}
			elems = append(elems, t)
		}
		return tftypes.Tuple{ElementTypes: elems}, nil
	}
	return nil, fmt.Errorf("unknown type kind %q", kind)
}

func snapshotPath(dir, suffix string, version int64) string {
	return filepath.Join(dir, suffix, fmt.Sprintf("v%d.json", version))
}

func loadSnapshot(dir, suffix string, version int64) (tftypes.Type, error) {
	path := snapshotPath(dir, suffix, version)
	raw, err := os.ReadFile(path) //nolint:gosec // a path under testdata
	if err != nil {
		return nil, err
	}
	typ, err := parseStateType(raw)
	if err != nil {
		return nil, fmt.Errorf("%s doesn't parse as a state type: %w", path, err)
	}
	return typ, nil
}

func readSnapshot(t *testing.T, suffix string, version int64) tftypes.Type {
	t.Helper()
	typ, err := loadSnapshot(stateSchemaDir, suffix, version)
	if err != nil {
		t.Fatalf("%s: %v", suffix, err)
	}
	return typ
}

// Every resource declares a version, and the version is the length of its
// history, so the version can't move without a step to migrate along.
func TestEveryResourceDeclaresItsStateHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	seen := map[string]bool{}
	for _, rr := range registeredResources(t) {
		seen[rr.suffix] = true
		h, ok := stateHistories[rr.suffix]
		if !ok {
			t.Errorf("%s has no entry in stateHistories", rr.typeName)
			continue
		}
		if h.version() < 1 {
			t.Errorf("%s is at schema version %d. Version 0 is what every build wrote before versions were declared, so a resource starts at 1", rr.typeName, h.version())
		}
		if rr.schema.Version != h.version() {
			t.Errorf("%s declares schema version %d and its history is %d steps long. Set Version from the history, never by hand", rr.typeName, rr.schema.Version, h.version())
		}
		for i, step := range h.steps {
			if strings.TrimSpace(step.why) == "" {
				t.Errorf("%s: the step from version %d says nothing about what it migrates", rr.typeName, i)
			}
		}

		upgrading, ok := rr.resource.(resource.ResourceWithUpgradeState)
		if !ok {
			t.Errorf("%s doesn't implement UpgradeState", rr.typeName)
			continue
		}
		var got []int64
		for v := range upgrading.UpgradeState(ctx) {
			got = append(got, v)
		}
		slices.Sort(got)
		if h.firstVersion < 0 || h.firstVersion > h.version() {
			t.Errorf("%s says its first release declared version %d, and its history runs 0 to %d", rr.typeName, h.firstVersion, h.version())
		}
		var want []int64
		for v := h.firstVersion; v < h.version(); v++ {
			want = append(want, v)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s upgrades from versions %v, want every prior version %v", rr.typeName, got, want)
		}

		_, moves := rr.resource.(resource.ResourceWithMoveState)
		switch {
		case len(h.formerNames) > 0 && !moves:
			t.Errorf("%s was once registered under another name and doesn't implement MoveState, so a moved block from that name fails", rr.typeName)
		case len(h.formerNames) == 0 && moves:
			t.Errorf("%s implements MoveState with no former name to move from", rr.typeName)
		}
	}
	for suffix := range stateHistories {
		if !seen[suffix] {
			t.Errorf("stateHistories has %q, which no registered resource answers to", suffix)
		}
	}
}

// The snapshot test. Read its failure message: it says what to do.
func TestStateSchemaSnapshots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, rr := range registeredResources(t) {
		problems := snapshotProblems(stateSchemaDir, rr.typeName, rr.suffix, stateHistories[rr.suffix],
			renderStateType(ctx, rr.schema), *updateStateSchemas, t.Logf)
		for _, p := range problems {
			t.Error(p)
		}
	}
}

// snapshotProblems checks one resource's committed snapshots under dir against
// its history and the shape its schema stores now, and says what's wrong in
// words that say what to do. With record set, a snapshot for the current
// version that doesn't exist yet is written rather than reported.
func snapshotProblems(dir, typeName, suffix string, h stateHistory, got []byte, record bool, logf func(string, ...any)) []string {
	current := h.version()
	path := snapshotPath(dir, suffix, current)

	want, err := os.ReadFile(path) //nolint:gosec // a path under testdata
	switch {
	case errors.Is(err, fs.ErrNotExist) && record:
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o750); mkErr != nil {
			return []string{mkErr.Error()}
		}
		if writeErr := os.WriteFile(path, got, 0o600); writeErr != nil {
			return []string{writeErr.Error()}
		}
		logf("%s: recorded %s", typeName, path)
		want = got
	case errors.Is(err, fs.ErrNotExist):
		return []string{fmt.Sprintf("%s is at schema version %d and %s doesn't exist. Record it with\n"+
			"  go test ./internal/provider -run TestStateSchemaSnapshots -update-state-schemas",
			typeName, current, path)}
	case err != nil:
		return []string{err.Error()}
	}

	// The snapshot is the shape a release stored at this version. The schema
	// may add what the history lists as added later, and nothing else.
	if problems := addedLaterProblems(typeName, path, current, want, got, h.addedAt(current)); problems != nil {
		return problems
	}

	// Every earlier version stays committed and parseable, because the
	// upgrade test below builds its prior states from them.
	var problems []string
	shapes := map[int64]tftypes.Type{}
	for v := h.firstVersion; v <= current; v++ {
		typ, loadErr := loadSnapshot(dir, suffix, v)
		switch {
		case errors.Is(loadErr, fs.ErrNotExist):
			problems = append(problems, fmt.Sprintf("%s: %s doesn't exist.\n"+
				"If the resource is new and no release has ever stored it at version %d, say so: set firstVersion: %d in its\n"+
				"stateHistories entry (state_history.go), so that nothing expects a state from before it.\n"+
				"If a release did store it there, restore the file from git: it's the shape in people's state files.",
				typeName, snapshotPath(dir, suffix, v), v, current))
			continue
		case loadErr != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", typeName, loadErr))
			continue
		}
		shapes[v] = typ
	}
	// Every attribute that leaves the shape at a step has to be accounted
	// for by that step, or a rename could drop a value with nothing failing.
	for v := h.firstVersion; v < current; v++ {
		before, haveBefore := shapes[v]
		after, haveAfter := shapes[v+1]
		if haveBefore && haveAfter {
			// What a build added to version v later is in some states at v,
			// so the step from v accounts for it too.
			if grown, errs := withAdded(before, h.addedAt(v)); len(errs) == 0 {
				before = grown
			} else {
				problems = append(problems, fmt.Sprintf("%s: version %d's addedLater: %s", typeName, v, strings.Join(errs, ", ")))
			}
			problems = append(problems, stepProblems(typeName, v, h.steps[v], before, after)...)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, suffix))
	if err != nil {
		return append(problems, err.Error())
	}
	for _, e := range entries {
		var v int64
		if _, err := fmt.Sscanf(e.Name(), "v%d.json", &v); err != nil || v > current || v < h.firstVersion {
			problems = append(problems, fmt.Sprintf("%s: %s doesn't belong: snapshots run v%d to v%d", typeName, e.Name(), h.firstVersion, current))
		}
	}
	return problems
}

// populated builds a JSON value of typ with every attribute set, so an upgrade
// that drops or garbles anything shows up as a difference.
func populated(typ tftypes.Type, at string) any {
	switch tt := typ.(type) {
	case tftypes.List:
		return []any{populated(tt.ElementType, at+"[0]")}
	case tftypes.Set:
		return []any{populated(tt.ElementType, at+"[0]")}
	case tftypes.Map:
		return map[string]any{"key": populated(tt.ElementType, at+"[key]")}
	case tftypes.Object:
		obj := map[string]any{}
		for name, attr := range tt.AttributeTypes {
			obj[name] = populated(attr, strings.TrimPrefix(at+"."+name, "."))
		}
		return obj
	case tftypes.Tuple:
		var out []any
		for i, el := range tt.ElementTypes {
			out = append(out, populated(el, fmt.Sprintf("%s[%d]", at, i)))
		}
		return out
	}
	switch {
	case typ.Is(tftypes.String):
		return "value-of-" + at
	case typ.Is(tftypes.Number):
		return json.Number("7")
	case typ.Is(tftypes.Bool):
		return true
	}
	return nil
}

func stringValues(values ...string) []tftypes.Value {
	out := make([]tftypes.Value, len(values))
	for i, v := range values {
		out[i] = tftypes.NewValue(tftypes.String, v)
	}
	return out
}

func protocolServer(t *testing.T) tfprotov6.ProviderServer {
	t.Helper()
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// currentStateType is what Terraform is told the resource's state looks like,
// asked the way Terraform asks.
func currentStateType(t *testing.T, server tfprotov6.ProviderServer, typeName string) (tftypes.Type, int64) {
	t.Helper()
	resp, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := resp.ResourceSchemas[typeName]
	if !ok {
		t.Fatalf("Terraform isn't told about %s", typeName)
	}
	return s.ValueType(), s.Version
}

func upgradeThroughProtocol(t *testing.T, server tfprotov6.ProviderServer, typeName string, version int64, state []byte) (tftypes.Value, []*tfprotov6.Diagnostic) {
	t.Helper()
	resp, err := server.UpgradeResourceState(context.Background(), &tfprotov6.UpgradeResourceStateRequest{
		TypeName: typeName,
		Version:  version,
		RawState: &tfprotov6.RawState{JSON: state},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.UpgradedState == nil {
		return tftypes.Value{}, resp.Diagnostics
	}
	typ, _ := currentStateType(t, server, typeName)
	got, err := resp.UpgradedState.Unmarshal(typ)
	if err != nil {
		t.Fatalf("%s: the upgraded state doesn't decode as the current shape: %v", typeName, err)
	}
	return got, resp.Diagnostics
}

func diagnosticErrors(diags []*tfprotov6.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			out = append(out, d.Summary+": "+d.Detail)
		}
	}
	return out
}

// Every prior version of every resource, fully populated from its committed
// snapshot, goes through UpgradeResourceState the way Terraform sends it on
// the first plan after a provider upgrade. Where nothing between that version
// and now changed the shape, nothing may change in the value either.
func TestEveryPriorStateVersionUpgradesThroughTheProtocol(t *testing.T) {
	t.Parallel()
	server := protocolServer(t)

	for _, rr := range registeredResources(t) {
		h := stateHistories[rr.suffix]
		typ, reported := currentStateType(t, server, rr.typeName)
		if reported != h.version() {
			t.Errorf("%s: Terraform is told schema version %d, want %d", rr.typeName, reported, h.version())
		}
		// The current version too: a state the last release wrote there
		// lacks whatever was added to it later, and has to read with each
		// of those null.
		for v := h.firstVersion; v <= h.version(); v++ {
			prior := populated(readSnapshot(t, rr.suffix, v), "")
			raw, err := json.Marshal(prior)
			if err != nil {
				t.Fatal(err)
			}
			got, diags := upgradeThroughProtocol(t, server, rr.typeName, v, raw)
			if errs := diagnosticErrors(diags); len(errs) > 0 {
				t.Errorf("%s: upgrading a version %d state failed: %v", rr.typeName, v, errs)
				continue
			}
			if !h.shapeUnchangedSince(v) {
				// A step that rewrites the state carries its own test, with the
				// before and after it was written for.
				continue
			}
			want, err := tftypes.ValueFromJSONWithOpts(raw, typ, tftypes.ValueFromJSONOpts{IgnoreUndefinedAttributes: true})
			if err != nil {
				t.Fatalf("%s: %v", rr.typeName, err)
			}
			if !got.Equal(want) {
				diffs, diffErr := want.Diff(got)
				t.Errorf("%s: upgrading from version %d changed the value, and no step since then changes the shape:\n%v %v", rr.typeName, v, diffs, diffErr)
			}
		}
	}
}

// A state this build wrote never has to upgrade, and Terraform still sends it
// through UpgradeResourceState on every read. It has to come back untouched.
func TestACurrentVersionStateComesBackUntouched(t *testing.T) {
	t.Parallel()
	server := protocolServer(t)

	for _, rr := range registeredResources(t) {
		typ, version := currentStateType(t, server, rr.typeName)
		raw, err := json.Marshal(populated(typ, ""))
		if err != nil {
			t.Fatal(err)
		}
		got, diags := upgradeThroughProtocol(t, server, rr.typeName, version, raw)
		if errs := diagnosticErrors(diags); len(errs) > 0 {
			t.Fatalf("%s: %v", rr.typeName, errs)
		}
		want, err := tftypes.ValueFromJSONWithOpts(raw, typ, tftypes.ValueFromJSONOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(want) {
			t.Errorf("%s: a current-version state changed on its way through", rr.typeName)
		}
	}
}

// A project as a pre-release build stored it, at the version every build wrote
// before versions were declared. retired_attribute stands for an attribute a
// pre-release build wrote that this one no longer has: it's dropped, and the
// upgrade doesn't fail over it.
func TestAVersionZeroProjectUpgradesWithItsValues(t *testing.T) {
	t.Parallel()
	server := protocolServer(t)
	typeName := ResourceName(projectResourceSuffix)

	const v0 = `{
  "allow_replace": null,
  "description": "created by the demo",
  "display_name": "Demo",
  "id": "tf-0123456789abcdef",
  "organization_id": "org-demo",
  "retired_attribute": "written by an older build",
  "self_link": "/v1/organizations/org-demo/projects/tf-0123456789abcdef",
  "status": "active",
  "timeouts": null
}`
	got, diags := upgradeThroughProtocol(t, server, typeName, 0, []byte(v0))
	if errs := diagnosticErrors(diags); len(errs) > 0 {
		t.Fatalf("upgrading failed: %v", errs)
	}

	var attrs map[string]tftypes.Value
	if err := got.As(&attrs); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"id":              "tf-0123456789abcdef",
		"organization_id": "org-demo",
		"display_name":    "Demo",
		"description":     "created by the demo",
		"status":          "active",
		"self_link":       "/v1/organizations/org-demo/projects/tf-0123456789abcdef",
	} {
		var s string
		if err := attrs[name].As(&s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s != want {
			t.Errorf("%s = %q, want %q", name, s, want)
		}
	}
	if !attrs["allow_replace"].IsNull() || !attrs["timeouts"].IsNull() {
		t.Error("an attribute stored as null came back set")
	}
	if _, kept := attrs["retired_attribute"]; kept {
		t.Error("an attribute this build doesn't have survived the upgrade")
	}
}

// A project stored before v1.3.0-dev.4 locked projects to a region has no
// `region`. It upgrades with every value kept and the region null, which the
// refresh after the upgrade fills from the project, and which a project the
// platform reports with no region keeps.
func TestAVersionOneProjectUpgradesWithNoRegion(t *testing.T) {
	t.Parallel()
	server := protocolServer(t)
	typeName := ResourceName(projectResourceSuffix)

	const v1 = `{
  "allow_replace": true,
  "description": "",
  "display_name": "Demo",
  "id": "tf-0123456789abcdef",
  "organization_id": "org-demo",
  "self_link": "/v1/regions/global/organizations/org-demo/projects/tf-0123456789abcdef",
  "status": "active",
  "timeouts": {"delete": "30m"}
}`
	got, diags := upgradeThroughProtocol(t, server, typeName, 1, []byte(v1))
	if errs := diagnosticErrors(diags); len(errs) > 0 {
		t.Fatalf("upgrading failed: %v", errs)
	}
	var attrs map[string]tftypes.Value
	if err := got.As(&attrs); err != nil {
		t.Fatal(err)
	}
	if !attrs["region"].IsNull() {
		t.Errorf("region = %v, want null", attrs["region"])
	}
	for name, want := range map[string]string{
		"id": "tf-0123456789abcdef", "organization_id": "org-demo", "display_name": "Demo", "status": "active",
	} {
		var s string
		if err := attrs[name].As(&s); err != nil || s != want {
			t.Errorf("%s = %q (%v), want %q", name, s, err, want)
		}
	}
	var allow bool
	if err := attrs["allow_replace"].As(&allow); err != nil || !allow {
		t.Errorf("allow_replace = %v (%v), want true", allow, err)
	}
}

// A state from a version newer than this build is refused rather than read as
// though it were current. That's what happens after a provider downgrade.
func TestANewerStateVersionIsRefused(t *testing.T) {
	t.Parallel()
	server := protocolServer(t)
	typeName := ResourceName(projectResourceSuffix)
	_, current := currentStateType(t, server, typeName)

	_, diags := upgradeThroughProtocol(t, server, typeName, current+1, []byte(`{"id":"tf-0123456789abcdef"}`))
	if len(diagnosticErrors(diags)) == 0 {
		t.Fatalf("a version %d state was accepted by a build at version %d", current+1, current)
	}
}

func moveThroughProtocol(t *testing.T, server tfprotov6.ProviderServer, sourceAddress, sourceType string, sourceVersion int64, target string, state []byte) (tftypes.Value, []*tfprotov6.Diagnostic) {
	t.Helper()
	resp, err := server.MoveResourceState(context.Background(), &tfprotov6.MoveResourceStateRequest{
		SourceProviderAddress: sourceAddress,
		SourceTypeName:        sourceType,
		SourceSchemaVersion:   sourceVersion,
		SourceState:           &tfprotov6.RawState{JSON: state},
		TargetTypeName:        target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.TargetState == nil {
		return tftypes.Value{}, resp.Diagnostics
	}
	typ, _ := currentStateType(t, server, target)
	got, err := resp.TargetState.Unmarshal(typ)
	if err != nil {
		t.Fatalf("%s: the moved state doesn't decode as the current shape: %v", target, err)
	}
	return got, resp.Diagnostics
}

// The pre-release address the cluster and the instance group were registered
// under before the provider type settled. Only the last segment is matched,
// so a local mirror under another namespace moves too.
const preReleaseAddress = "registry.terraform.io/example-user/" + preReleaseProviderTypeName

// A cluster as the pre-release build stored it under its old type name, with
// the attributes that build had and nothing since. A `moved` block from that
// name lands it on the current type with every value it had, and with the
// attributes added since then null for the next refresh to fill.
func TestAMovedBlockFromThePreReleaseClusterTypeKeepsItsValues(t *testing.T) {
	t.Parallel()
	server := protocolServer(t)

	const old = `{
  "cluster_type": "/v1/regions/us-texas-1/compute/cluster-types/gpu-training",
  "cluster_type_resolved": "/v1/regions/us-texas-1/compute/cluster-types/gpu-training/versions/3",
  "description": null,
  "display_name": "Training",
  "id": "tf-demo",
  "node_count": 2,
  "node_pool": {"gpu-workers": {"node_count": 2}},
  "self_link": "/v1/regions/us-texas-1/projects/demo/compute/clusters/tf-demo",
  "state": "ready",
  "tags": {"team": "ml"}
}`
	got, diags := moveThroughProtocol(t, server, preReleaseAddress,
		preReleaseProviderTypeName+"_"+clusterResourceSuffix, 0,
		ResourceName(clusterResourceSuffix), []byte(old))
	if errs := diagnosticErrors(diags); len(errs) > 0 {
		t.Fatalf("the move failed: %v", errs)
	}

	var attrs map[string]tftypes.Value
	if err := got.As(&attrs); err != nil {
		t.Fatal(err)
	}
	if want := stringValues("tf-demo", "Training"); !attrs["id"].Equal(want[0]) || !attrs["display_name"].Equal(want[1]) {
		t.Errorf("id, display_name = %v, %v, want tf-demo, Training", attrs["id"], attrs["display_name"])
	}
	var pools map[string]tftypes.Value
	if err := attrs["node_pool"].As(&pools); err != nil {
		t.Fatal(err)
	}
	var pool map[string]tftypes.Value
	if err := pools["gpu-workers"].As(&pool); err != nil {
		t.Fatal(err)
	}
	if !pool["node_count"].Equal(tftypes.NewValue(tftypes.Number, 2)) {
		t.Errorf("node_pool[gpu-workers].node_count = %v, want 2", pool["node_count"])
	}
	for _, added := range []string{"project", "region", "vpc", "security_groups", "allow_replace"} {
		if !attrs[added].IsNull() {
			t.Errorf("%s, which the pre-release build never had, came back %v", added, attrs[added])
		}
	}
}

// After `terraform state replace-provider` the old type name sits under this
// provider's address, and the moved block still lands it.
func TestAMovedBlockAfterReplaceProviderKeepsItsValues(t *testing.T) {
	t.Parallel()
	server := protocolServer(t)

	got, diags := moveThroughProtocol(t, server, RegistryAddress(),
		preReleaseProviderTypeName+"_"+clusterResourceSuffix, 0,
		ResourceName(clusterResourceSuffix), []byte(`{"id": "tf-demo", "state": "ready"}`))
	if errs := diagnosticErrors(diags); len(errs) > 0 {
		t.Fatalf("the move failed: %v", errs)
	}
	var attrs map[string]tftypes.Value
	if err := got.As(&attrs); err != nil {
		t.Fatal(err)
	}
	if !attrs["id"].Equal(tftypes.NewValue(tftypes.String, "tf-demo")) {
		t.Errorf("id = %v, want tf-demo", attrs["id"])
	}
}

func TestAMovedBlockFromThePreReleaseInstanceGroupTypeKeepsItsValues(t *testing.T) {
	t.Parallel()
	server := protocolServer(t)

	const old = `{"id": "tf-pool", "node_pool": {"workers": {"size": 3}}, "state": "active"}`
	got, diags := moveThroughProtocol(t, server, preReleaseAddress,
		preReleaseProviderTypeName+"_"+instanceGroupResourceSuffix, 0,
		ResourceName(instanceGroupResourceSuffix), []byte(old))
	if errs := diagnosticErrors(diags); len(errs) > 0 {
		t.Fatalf("the move failed: %v", errs)
	}
	var attrs map[string]tftypes.Value
	if err := got.As(&attrs); err != nil {
		t.Fatal(err)
	}
	if !attrs["id"].Equal(tftypes.NewValue(tftypes.String, "tf-pool")) {
		t.Errorf("id = %v, want tf-pool", attrs["id"])
	}
}

// Anything that isn't a former name of the target is refused, so a moved block
// can't turn one kind of resource into another.
func TestAMovedBlockFromAnythingElseIsRefused(t *testing.T) {
	t.Parallel()
	server := protocolServer(t)
	cluster := ResourceName(clusterResourceSuffix)

	for _, tc := range []struct{ name, address, typeName string }{
		{"another provider's resource", "registry.terraform.io/hashicorp/random", "random_string"},
		{"the pre-release instance group, onto a cluster", preReleaseAddress, preReleaseProviderTypeName + "_" + instanceGroupResourceSuffix},
		{"the old type name from a provider with another name", "registry.terraform.io/example-user/other", preReleaseProviderTypeName + "_" + clusterResourceSuffix},
		{"one of this provider's own types", RegistryAddress(), ResourceName(instanceGroupResourceSuffix)},
	} {
		_, diags := moveThroughProtocol(t, server, tc.address, tc.typeName, 0, cluster, []byte(`{"id":"x"}`))
		if len(diagnosticErrors(diags)) == 0 {
			t.Errorf("%s: moving it onto %s was accepted", tc.name, cluster)
		}
	}

	// The type with no former name refuses everything, the pre-release name
	// that matches its suffix included: no such type ever existed.
	project := ResourceName(projectResourceSuffix)
	_, diags := moveThroughProtocol(t, server, preReleaseAddress,
		preReleaseProviderTypeName+"_"+projectResourceSuffix, 0, project, []byte(`{"id":"x"}`))
	if len(diagnosticErrors(diags)) == 0 {
		t.Errorf("a move onto %s was accepted, and it has no former name", project)
	}
}

// A step's error stops the upgrade with the version and the reason, rather
// than writing a half-migrated state.
func TestAFailingStepStopsTheUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := stateHistory{steps: []stateStep{
		{why: "nothing changed"},
		{why: "id became required", apply: func(state map[string]any) error {
			if state["id"] == nil {
				return errors.New("the state has no id")
			}
			return nil
		}},
	}}
	if h.shapeUnchangedSince(0) || !h.shapeUnchangedSince(2) {
		t.Fatal("shapeUnchangedSince misreads the history")
	}

	var rr registeredResource
	for _, r := range registeredResources(t) {
		if r.suffix == projectResourceSuffix {
			rr = r
		}
	}
	upgraders := h.upgraders()
	keys := make([]int, 0, len(upgraders))
	for v := range upgraders {
		keys = append(keys, int(v))
	}
	sort.Ints(keys)
	if !slices.Equal(keys, []int{0, 1}) {
		t.Fatalf("upgraders from %v, want [0 1]", keys)
	}

	resp := resource.UpgradeStateResponse{}
	resp.State.Schema = rr.schema
	upgraders[0].StateUpgrader(ctx, resource.UpgradeStateRequest{
		RawState: &tfprotov6.RawState{JSON: []byte(`{"display_name":"no id"}`)},
	}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a step that failed let the upgrade through")
	}
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, "version 1 to 2") || !strings.Contains(detail, "the state has no id") {
		t.Errorf("the error doesn't say which step failed or why: %s", detail)
	}
}

// writeSnapshots commits a made-up resource's snapshots, one per version, to a
// directory of its own, for the guard to be tried against.
func writeSnapshots(t *testing.T, suffix string, versions map[int64]string) string {
	t.Helper()
	dir := t.TempDir()
	for v, shape := range versions {
		path := snapshotPath(dir, suffix, v)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(shape), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const (
	shapeWithOldName = `["object",{"id":"string","old_name":"string"}]` + "\n"
	shapeWithNewName = `["object",{"id":"string","new_name":"string"}]` + "\n"
)

// A rename written as a remove and an add, with no step to carry the value,
// would pass every other test: the old attribute is dropped as undefined and
// the new one reads as null. The guard has to catch it.
func TestARenameWithNoStepToCarryItIsCaught(t *testing.T) {
	t.Parallel()
	dir := writeSnapshots(t, "renamed", map[int64]string{0: shapeWithOldName, 1: shapeWithNewName})
	h := stateHistory{steps: []stateStep{{why: "old_name is now new_name"}}}

	problems := snapshotProblems(dir, ResourceName("renamed"), "renamed", h, []byte(shapeWithNewName), false, t.Logf)
	if len(problems) == 0 {
		t.Fatal("a rename with no step to carry the value passed the guard, so the value would be dropped")
	}
	if !strings.Contains(strings.Join(problems, "\n"), "old_name") {
		t.Errorf("the problem doesn't name the attribute that went: %v", problems)
	}
}

// stepProblems checks that the step from version v accounts for every
// attribute that leaves the stored shape between before and after, and that
// its apply carries each value it says it moves.
func stepProblems(typeName string, v int64, step stateStep, before, after tftypes.Type) []string {
	var problems []string
	gone := removedAttributes(before, after, "")
	named := map[string]bool{}
	for _, path := range step.dropped {
		named[path] = true
	}
	for path := range step.moved {
		named[path] = true
	}
	for _, path := range gone {
		if !named[path] {
			problems = append(problems, fmt.Sprintf("%s: %s is in v%d.json and not in v%d.json, and the step from version %d to %d doesn't say what happens to its value.\n"+
				"If it was renamed or moved, write the step's apply to carry the value and name it in moved. If its value is discarded on purpose,\n"+
				"name it in dropped. Otherwise every state that upgrades through this step loses it without a word.",
				typeName, path, v, v+1, v, v+1))
		}
	}
	for path := range named {
		if !slices.Contains(gone, path) {
			problems = append(problems, fmt.Sprintf("%s: the step from version %d to %d names %s as dropped or moved, and it doesn't leave the shape", typeName, v, v+1, path))
		}
	}
	if len(step.moved) == 0 {
		return problems
	}
	if step.apply == nil {
		return append(problems, fmt.Sprintf("%s: the step from version %d to %d moves %d attributes and has no apply to carry them", typeName, v, v+1, len(step.moved)))
	}

	// populated gives every string a value of its own path, so a value that
	// lands in the wrong place, or nowhere, shows.
	original, isObject := populated(before, "").(map[string]any)
	state, alsoObject := populated(before, "").(map[string]any)
	if !isObject || !alsoObject {
		return append(problems, fmt.Sprintf("%s: the version %d state isn't an object", typeName, v))
	}
	if err := step.apply(state); err != nil {
		return append(problems, fmt.Sprintf("%s: the step from version %d to %d fails on a fully populated state: %v", typeName, v, v+1, err))
	}
	for from, to := range step.moved {
		want, _ := valueAt(before, original, strings.Split(from, "."))
		got, ok := valueAt(after, state, strings.Split(to, "."))
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: the step from version %d to %d moves %s to %s, which isn't in v%d.json or isn't set after the step", typeName, v, v+1, from, to, v+1))
		case !reflect.DeepEqual(got, want):
			problems = append(problems, fmt.Sprintf("%s: the step from version %d to %d moves %s to %s, and after it %s holds %v, not %v", typeName, v, v+1, from, to, to, got, want))
		}
	}
	return problems
}

// addedLaterProblems compares the shape the schema stores now with the
// snapshot of its version plus added, and says what to do when they differ.
// It's nil when they match.
func addedLaterProblems(typeName, path string, current int64, want, got []byte, added map[string]string) []string {
	mismatch := func(extra string) []string {
		return []string{fmt.Sprintf("%s: the shape its state is stored in no longer matches %s, the snapshot for schema version %d.\n"+
			"A state written by the last release would no longer be read the way it was written.%s\n"+
			"If the schema only adds attributes, put each in addedLater on version %d's step in stateHistories\n"+
			"(state_history.go), with its type. The last release then still reads this build's state, and this build\n"+
			"reads the last release's with each added attribute null.\n"+
			"Anything else, a removal, a rename or a type change, needs a step that migrates version %d to %d, which\n"+
			"raises the schema version, then a record of v%d.json with\n"+
			"  go test ./internal/provider -run TestStateSchemaSnapshots -update-state-schemas\n"+
			"Never edit %s: it's the shape that's in people's state files.\n\n--- %s\n%s\n--- the schema now\n%s",
			typeName, path, current, extra, current, current, current+1, current+1, path, path, want, got)}
	}
	wantType, err := parseStateType(want)
	if err != nil {
		return []string{fmt.Sprintf("%s doesn't parse as a state type: %v", path, err)}
	}
	grown, errs := withAdded(wantType, added)
	if len(errs) > 0 {
		return []string{fmt.Sprintf("%s: version %d's addedLater: %s", typeName, current, strings.Join(errs, ", "))}
	}
	if bytes.Equal(got, want) && len(added) == 0 {
		return nil
	}
	gotType, err := parseStateType(got)
	if err != nil {
		return []string{err.Error()}
	}
	if !grown.Equal(gotType) {
		return mismatch("")
	}
	return nil
}

// withAdded is typ with each attribute in added put at its path with the
// type it names. Each path names an attribute typ doesn't have, under objects
// typ does, looking through collections as removedAttributes does.
func withAdded(typ tftypes.Type, added map[string]string) (tftypes.Type, []string) {
	paths := make([]string, 0, len(added))
	for p := range added {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var errs []string
	for _, p := range paths {
		declared, err := parseStateType([]byte(added[p]))
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s's type %s doesn't parse: %v", p, added[p], err))
			continue
		}
		next, err := addAttribute(typ, strings.Split(p, "."), declared)
		if err != nil {
			errs = append(errs, p+" "+err.Error())
			continue
		}
		typ = next
	}
	return typ, errs
}

func addAttribute(typ tftypes.Type, path []string, declared tftypes.Type) (tftypes.Type, error) {
	switch tt := typ.(type) {
	case tftypes.List:
		e, err := addAttribute(tt.ElementType, path, declared)
		return tftypes.List{ElementType: e}, err
	case tftypes.Set:
		e, err := addAttribute(tt.ElementType, path, declared)
		return tftypes.Set{ElementType: e}, err
	case tftypes.Map:
		e, err := addAttribute(tt.ElementType, path, declared)
		return tftypes.Map{ElementType: e}, err
	case tftypes.Object:
		attrs := make(map[string]tftypes.Type, len(tt.AttributeTypes)+1)
		for name, at := range tt.AttributeTypes {
			attrs[name] = at
		}
		was, inSnapshot := attrs[path[0]]
		switch {
		case len(path) == 1 && inSnapshot:
			return nil, errors.New("is in the snapshot already")
		case len(path) == 1:
			attrs[path[0]] = declared
		case !inSnapshot:
			return nil, errors.New("sits under an attribute the snapshot doesn't have: add that one instead")
		default:
			inner, err := addAttribute(was, path[1:], declared)
			if err != nil {
				return nil, err
			}
			attrs[path[0]] = inner
		}
		return tftypes.Object{AttributeTypes: attrs}, nil
	}
	return nil, errors.New("isn't under an object")
}

// removedAttributes is the path of every attribute in before that isn't in
// after, outermost first: a block that went is named and its insides aren't.
// A collection's elements are looked through, so an attribute of a block
// stored as a list is named by the block's path and its own.
func removedAttributes(before, after tftypes.Type, prefix string) []string {
	before, after = elementOf(before), elementOf(after)
	b, ok := before.(tftypes.Object)
	if !ok {
		return nil
	}
	a, ok := after.(tftypes.Object)
	if !ok {
		return nil
	}
	var out []string
	for name, typ := range b.AttributeTypes {
		path := prefix + name
		next, kept := a.AttributeTypes[name]
		if !kept {
			out = append(out, path)
			continue
		}
		out = append(out, removedAttributes(typ, next, path+".")...)
	}
	sort.Strings(out)
	return out
}

// elementOf looks through lists, sets and maps to what they hold.
func elementOf(typ tftypes.Type) tftypes.Type {
	for {
		switch tt := typ.(type) {
		case tftypes.List:
			typ = tt.ElementType
		case tftypes.Set:
			typ = tt.ElementType
		case tftypes.Map:
			typ = tt.ElementType
		default:
			return typ
		}
	}
}

// valueAt finds the value at path in a state of type typ built by populated,
// looking through a collection to its one element.
func valueAt(typ tftypes.Type, value any, path []string) (any, bool) {
	for len(path) > 0 {
		switch tt := typ.(type) {
		case tftypes.List, tftypes.Set:
			l, ok := value.([]any)
			if !ok || len(l) == 0 {
				return nil, false
			}
			typ, value = elementTypeOf(tt), l[0]
			continue
		case tftypes.Map:
			m, ok := value.(map[string]any)
			if !ok {
				return nil, false
			}
			typ, value = tt.ElementType, m["key"]
			continue
		case tftypes.Object:
			attr, ok := tt.AttributeTypes[path[0]]
			m, isObject := value.(map[string]any)
			if !ok || !isObject {
				return nil, false
			}
			held, set := m[path[0]]
			if !set {
				return nil, false
			}
			typ, value, path = attr, held, path[1:]
			continue
		}
		return nil, false
	}
	return value, value != nil
}

func elementTypeOf(typ tftypes.Type) tftypes.Type {
	if l, ok := typ.(tftypes.List); ok {
		return l.ElementType
	}
	if s, ok := typ.(tftypes.Set); ok {
		return s.ElementType
	}
	return typ
}

// The ways a step can account for an attribute that goes, and the ways it can
// get that wrong.
func TestAStepAccountsForEveryAttributeThatGoes(t *testing.T) {
	t.Parallel()
	carry := func(state map[string]any) error {
		state["new_name"] = state["old_name"]
		delete(state, "old_name")
		return nil
	}
	const (
		blockBefore = `["object",{"id":"string","pools":["list",["object",{"size":"number","zone":"string"}]]}]` + "\n"
		blockAfter  = `["object",{"id":"string","pools":["list",["object",{"size":"number"}]]}]` + "\n"
	)
	for _, tc := range []struct {
		name          string
		before, after string
		step          stateStep
		want          string // a problem containing this, or "" for none
	}{
		{
			name: "a rename carried by apply", before: shapeWithOldName, after: shapeWithNewName,
			step: stateStep{why: "renamed", apply: carry, moved: map[string]string{"old_name": "new_name"}},
		},
		{
			name: "an attribute discarded on purpose", before: shapeWithOldName, after: shapeWithNewName,
			step: stateStep{why: "old_name went and new_name came", dropped: []string{"old_name"}},
		},
		{
			name: "a move with no apply", before: shapeWithOldName, after: shapeWithNewName,
			step: stateStep{why: "renamed", moved: map[string]string{"old_name": "new_name"}},
			want: "no apply to carry them",
		},
		{
			name: "a move whose apply loses the value", before: shapeWithOldName, after: shapeWithNewName,
			step: stateStep{why: "renamed", moved: map[string]string{"old_name": "new_name"}, apply: func(state map[string]any) error {
				delete(state, "old_name")
				return nil
			}},
			want: "isn't set after the step",
		},
		{
			name: "a move whose apply puts the wrong value there", before: shapeWithOldName, after: shapeWithNewName,
			step: stateStep{why: "renamed", moved: map[string]string{"old_name": "new_name"}, apply: func(state map[string]any) error {
				state["new_name"] = state["id"]
				return nil
			}},
			want: "holds value-of-id, not value-of-old_name",
		},
		{
			name: "dropped naming an attribute that stays", before: shapeWithOldName, after: shapeWithNewName,
			step: stateStep{why: "nothing", dropped: []string{"old_name", "id"}},
			want: "names id as dropped or moved, and it doesn't leave the shape",
		},
		{
			name: "an attribute inside a block, unaccounted for", before: blockBefore, after: blockAfter,
			step: stateStep{why: "zone went"},
			want: "pools.zone is in v0.json and not in v1.json",
		},
		{
			name: "an attribute inside a block, dropped", before: blockBefore, after: blockAfter,
			step: stateStep{why: "zone went", dropped: []string{"pools.zone"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := writeSnapshots(t, "made_up", map[int64]string{0: tc.before, 1: tc.after})
			h := stateHistory{steps: []stateStep{tc.step}}
			problems := strings.Join(snapshotProblems(dir, ResourceName("made_up"), "made_up", h, []byte(tc.after), false, t.Logf), "\n")
			switch {
			case tc.want == "" && problems != "":
				t.Errorf("the guard refused a step that accounts for everything:\n%s", problems)
			case tc.want != "" && !strings.Contains(problems, tc.want):
				t.Errorf("want a problem saying %q, got:\n%s", tc.want, problems)
			}
		})
	}
}

// A resource added after versions were declared does what the guard says, and
// ends up passing it. No release ever stored it at version 0, so there's no
// v0.json to commit, and recording the current version has to be enough.
func TestANewResourceCanFollowTheGuardsInstructions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const shape = `["object",{"id":"string"}]` + "\n"
	h := stateHistory{steps: []stateStep{versionDeclared}}

	first := strings.Join(snapshotProblems(dir, ResourceName("brand_new"), "brand_new", h, []byte(shape), true, t.Logf), "\n")
	if !strings.Contains(first, "firstVersion: 1") {
		t.Fatalf("a new resource's first run doesn't say how to mark it new:\n%s", first)
	}

	h.firstVersion = 1
	if problems := snapshotProblems(dir, ResourceName("brand_new"), "brand_new", h, []byte(shape), false, t.Logf); len(problems) > 0 {
		t.Fatalf("a new resource that did what the guard said still fails it:\n%s", strings.Join(problems, "\n"))
	}

	// Nothing ever stored it at 0, so there's no upgrader from 0, and a
	// state claiming to be from there is refused rather than read.
	if _, ok := h.upgraders()[0]; ok {
		t.Error("a resource first released at version 1 has an upgrader from 0")
	}
	var rr registeredResource
	for _, r := range registeredResources(t) {
		if r.suffix == projectResourceSuffix {
			rr = r
		}
	}
	var out tfsdk.State
	out.Schema = rr.schema
	diags := h.migrate(context.Background(), &tfprotov6.RawState{JSON: []byte(`{"id":"x"}`)}, 0, &out)
	if !diags.HasError() || !strings.Contains(diags.Errors()[0].Detail(), "below version 1") {
		t.Errorf("a version 0 state of a resource first released at 1 wasn't refused for that: %v", diags)
	}

	// Once it's out, the next shape change works the same as for any other.
	if writeErr := os.WriteFile(snapshotPath(dir, "brand_new", 0), []byte(shape), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if problems := strings.Join(snapshotProblems(dir, ResourceName("brand_new"), "brand_new", h, []byte(shape), false, t.Logf), "\n"); !strings.Contains(problems, "v0.json doesn't belong") {
		t.Errorf("a snapshot from before the resource's first version wasn't flagged: %s", problems)
	}
}

// An attribute added at the current version passes once addedLater names it,
// and only then. Everything else the guard refused before, a removal, a type
// change, an unnamed addition, it still refuses, and addedLater can't name an
// attribute the snapshot has or the schema doesn't.
func TestAnAttributeAddedLaterNeedsNoVersion(t *testing.T) {
	t.Parallel()
	const (
		v1        = `["object",{"id":"string","pools":["list",["object",{"size":"number"}]]}]` + "\n"
		added     = `["object",{"id":"string","keep":"string","pools":["list",["object",{"size":"number"}]]}]` + "\n"
		addedDeep = `["object",{"id":"string","pools":["list",["object",{"size":"number","zone":"string"}]]}]` + "\n"
		retyped   = `["object",{"id":"number","keep":"string","pools":["list",["object",{"size":"number"}]]}]` + "\n"
		removed   = `["object",{"keep":"string","pools":["list",["object",{"size":"number"}]]}]` + "\n"
	)
	for _, tc := range []struct {
		name, now  string
		addedLater map[string]string
		want       string // "" passes
	}{
		{"named", added, map[string]string{"keep": `"string"`}, ""},
		{"named inside a block", addedDeep, map[string]string{"pools.zone": `"string"`}, ""},
		{"not named", added, nil, "put each in addedLater"},
		{"named with another type", added, map[string]string{"keep": `"bool"`}, "no longer matches"},
		{"named, with a type change beside it", retyped, map[string]string{"keep": `"string"`}, "no longer matches"},
		{"named, with a removal beside it", removed, map[string]string{"keep": `"string"`}, "no longer matches"},
		{"named, but the snapshot has it", v1, map[string]string{"id": `"string"`}, "is in the snapshot already"},
		{"named, but the schema doesn't have it", added, map[string]string{"keep": `"string"`, "other": `"string"`}, "no longer matches"},
		{"named with a type that doesn't parse", added, map[string]string{"keep": `"strng"`}, "doesn't parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := writeSnapshots(t, "made_up", map[int64]string{1: v1})
			h := stateHistory{firstVersion: 1, steps: []stateStep{{why: "declared", addedLater: tc.addedLater}}}
			problems := strings.Join(snapshotProblems(dir, ResourceName("made_up"), "made_up", h, []byte(tc.now), false, t.Logf), "\n")
			switch {
			case tc.want == "" && problems != "":
				t.Errorf("refused an addition addedLater names:\n%s", problems)
			case tc.want != "" && !strings.Contains(problems, tc.want):
				t.Errorf("want a problem saying %q, got:\n%s", tc.want, problems)
			}
		})
	}
}

// An attribute added later at version 1 is in some states at 1, so the step
// that takes it away at 2 has to account for it like any other.
func TestAStepAccountsForWhatWasAddedLater(t *testing.T) {
	t.Parallel()
	const (
		v1 = `["object",{"id":"string"}]` + "\n"
		v2 = `["object",{"id":"string"}]` + "\n"
	)
	for _, tc := range []struct {
		name    string
		dropped []string
		want    string
	}{
		{"unaccounted", nil, "keep is in v1.json and not in v2.json"},
		{"dropped", []string{"keep"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := writeSnapshots(t, "made_up", map[int64]string{1: v1, 2: v2})
			h := stateHistory{firstVersion: 1, steps: []stateStep{
				{why: "declared", addedLater: map[string]string{"keep": `"string"`}},
				{why: "keep went", dropped: tc.dropped},
			}}
			problems := strings.Join(snapshotProblems(dir, ResourceName("made_up"), "made_up", h, []byte(v2), false, t.Logf), "\n")
			switch {
			case tc.want == "" && problems != "":
				t.Errorf("refused a step that drops what was added later:\n%s", problems)
			case tc.want != "" && !strings.Contains(problems, tc.want):
				t.Errorf("want a problem saying %q, got:\n%s", tc.want, problems)
			}
		})
	}
}
