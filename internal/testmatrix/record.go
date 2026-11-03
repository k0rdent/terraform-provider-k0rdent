// Package testmatrix records which cells of the testing matrix the acceptance
// tests exercise, from what the provider is asked to do rather than from
// anything a test declares.
//
// With a record file named, every provider server is wrapped in a recorder
// that watches the RPCs Terraform makes and appends a line for each cell a
// test exercised:
//
//   - import: a plan straight after an import and its read, for each
//     attribute the configuration sets or state holds, where the plan keeps
//     the imported value. A write-only attribute counts when the
//     configuration sets it.
//   - update: a plan with prior state and no error, where the test changed
//     the attribute's configuration since its last plan, the planned value
//     differs from the prior one (for a write-only attribute, the
//     configuration change is enough), and no RequiresReplace path covers it.
//   - replace: the same, where a RequiresReplace path does cover it.
//   - upgrade: an UpgradeResourceState from an older schema version, for each
//     attribute the upgraded state holds.
//
// The acceptance tests wrap the servers they start in process
// (internal/acceptance/matrix_record_test.go). A test that runs a built
// provider binary sets RecordEnv and TestEnv in its environment, and main.go
// serves through Serve. internal/repocheck turns the record into
// docs/testing-matrix.md.
//
// Each Terraform command starts a new provider, so what a test has done so
// far (an import waiting for its plan, the configuration its last plan saw)
// is kept in files beside the record, by test, type and id.
package testmatrix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/tf6server"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// RecordEnv names the record file a built provider binary appends to. Unset,
// the binary serves as it always does.
var RecordEnv = provider.EnvVar("TEST_MATRIX_RECORD")

// TestEnv names the top-level test a built provider binary runs for.
var TestEnv = provider.EnvVar("TEST_MATRIX_TEST")

// UnknownTest is what a binary records under when TestEnv isn't set. The
// check refuses it, so a test that starts a binary without saying which test
// it is fails rather than losing its cells.
const UnknownTest = "unknown"

// The cells a line can name.
const (
	CellImport  = "import"
	CellUpdate  = "update"
	CellReplace = "replace"
	CellUpgrade = "upgrade"
)

// Line is one recorded cell.
type Line struct {
	Test     string `json:"test"`
	Resource string `json:"resource"`
	Path     string `json:"path"`
	Cell     string `json:"cell"`
}

// server is what the framework's protocol 6 server implements. The recorder
// keeps both optional interfaces, or tf6server would stop serving list
// resources and actions through it.
type server interface {
	tfprotov6.ProviderServerWithListResource //nolint:staticcheck // what tf6server asserts today
	tfprotov6.ProviderServerWithActions      //nolint:staticcheck // what tf6server asserts today
}

// Wrap returns s with the recorder around it, appending to file under test.
func Wrap(s tfprotov6.ProviderServer, file, test string) (tfprotov6.ProviderServer, error) {
	full, ok := s.(server)
	if !ok {
		return nil, fmt.Errorf("the provider server %T doesn't serve list resources and actions, so the recorder can't wrap it", s)
	}
	if !filepath.IsAbs(file) {
		return nil, fmt.Errorf("the record file %q isn't an absolute path, so a binary run elsewhere would write somewhere else", file)
	}
	return &recorder{server: full, test: test, file: file}, nil
}

// Serve is providerserver.Serve with the recorder around each server, for a
// built binary run with RecordEnv set.
func Serve(address string, newProvider func() fwprovider.Provider, file, test string, debug bool) error {
	if test == "" {
		test = UnknownTest
	}
	var opts []tf6server.ServeOpt
	if debug {
		opts = append(opts, tf6server.WithManagedDebug())
	}
	// Wrapped once up front, so a server the recorder can't wrap stops the
	// binary rather than serving it unrecorded.
	if _, err := Wrap(providerserver.NewProtocol6(newProvider())(), file, test); err != nil {
		return err
	}
	return tf6server.Serve(address, func() tfprotov6.ProviderServer {
		s, err := Wrap(providerserver.NewProtocol6(newProvider())(), file, test)
		if err != nil {
			panic(err) // the same wrap as above, which succeeded
		}
		return s
	}, opts...)
}

type recorder struct {
	server
	test, file string

	mu      sync.Mutex
	schemas map[string]*tfprotov6.Schema
}

// memory is what a test has done to one object so far. Stage is where it is
// between an import and the plan after it: "imported", then "read". Config
// is the configuration its last plan or apply saw, and HasConfig says there
// was one.
type memory struct {
	Stage     string            `json:"stage,omitempty"`
	Config    map[string]string `json:"config,omitempty"`
	HasConfig bool              `json:"has_config,omitempty"`
}

// The recorders in one process share these, so two servers a test holds at
// once don't race on a memory file or a record line.
var (
	memoryMu sync.Mutex
	recordMu sync.Mutex
)

func (r *recorder) memoryFile(typ, id string) string {
	sum := sha256.Sum256([]byte(r.test + "\x00" + typ + "\x00" + id))
	return filepath.Join(r.file+".memory", hex.EncodeToString(sum[:])+".json")
}

// remember loads an object's memory, lets change edit it, and saves it.
func (r *recorder) remember(typ, id string, change func(*memory)) error {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	file := r.memoryFile(typ, id)
	var m memory
	b, readErr := os.ReadFile(file)
	switch {
	case readErr == nil:
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("reading %s: %w", file, err)
		}
	case !errors.Is(readErr, fs.ErrNotExist):
		return readErr
	}
	before := m
	change(&m)
	if m.Stage == before.Stage && m.HasConfig == before.HasConfig && maps.Equal(m.Config, before.Config) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

func (r *recorder) emit(typ, cell string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, p := range paths {
		if err := enc.Encode(Line{Test: r.test, Resource: typ, Path: p, Cell: cell}); err != nil {
			return err
		}
	}
	recordMu.Lock()
	defer recordMu.Unlock()
	f, err := os.OpenFile(r.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// One write, so lines from processes appending at once don't interleave.
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close() //nolint:errcheck // the write's error is the one to report
		return err
	}
	return f.Close()
}

// failed is the RPC's result with the recorder's own error added, so a cell
// the recorder couldn't write fails the test rather than leaving a gap.
func failed[T any](resp T, err, recordErr error) (T, error) {
	if recordErr != nil {
		return resp, errors.Join(err, fmt.Errorf("testing matrix recorder: %w", recordErr))
	}
	return resp, err
}

func (r *recorder) schema(ctx context.Context, typ string) *tfprotov6.Schema {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.schemas == nil {
		resp, err := r.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
		if err != nil || resp == nil {
			return nil
		}
		r.schemas = resp.ResourceSchemas
	}
	return r.schemas[typ]
}

func (r *recorder) decode(ctx context.Context, typ string, v *tfprotov6.DynamicValue) (map[string]string, bool) {
	s := r.schema(ctx, typ)
	if s == nil || v == nil {
		return nil, false
	}
	val, err := v.Unmarshal(s.ValueType())
	if err != nil || val.IsNull() {
		return nil, false
	}
	return Flatten(val), true
}

// Flatten gives every attribute path in a value, nested names joined by dots
// and the elements of a list, set or map of objects folded together. Each
// path's value is its leaves, sorted, so a set read back in another order is
// the same value. Null values are left out.
func Flatten(v tftypes.Value) map[string]string {
	found := map[string][]string{}
	var walk func(v tftypes.Value, prefix string) []string
	walk = func(v tftypes.Value, prefix string) []string {
		if v.IsNull() {
			return nil
		}
		if !v.IsKnown() {
			return []string{"<unknown>"}
		}
		typ := v.Type()
		switch {
		case typ.Is(tftypes.Object{}):
			var m map[string]tftypes.Value
			if err := v.As(&m); err != nil {
				return []string{"<" + err.Error() + ">"}
			}
			var out []string
			for k, e := range m {
				if e.IsNull() {
					continue
				}
				leaves := walk(e, prefix+k+".")
				sort.Strings(leaves)
				found[prefix+k] = append(found[prefix+k], strings.Join(leaves, "\x01"))
				for _, l := range leaves {
					out = append(out, k+"="+l)
				}
			}
			return out
		case typ.Is(tftypes.Map{}):
			var m map[string]tftypes.Value
			if err := v.As(&m); err != nil {
				return []string{"<" + err.Error() + ">"}
			}
			var out []string
			for k, e := range m {
				leaves := walk(e, prefix)
				sort.Strings(leaves)
				out = append(out, k+":"+strings.Join(leaves, "\x01"))
			}
			return out
		case typ.Is(tftypes.List{}), typ.Is(tftypes.Set{}), typ.Is(tftypes.Tuple{}):
			var l []tftypes.Value
			if err := v.As(&l); err != nil {
				return []string{"<" + err.Error() + ">"}
			}
			var out []string
			for i, e := range l {
				leaves := walk(e, prefix)
				sort.Strings(leaves)
				el := strings.Join(leaves, "\x01")
				if typ.Is(tftypes.List{}) {
					el = fmt.Sprint(i) + ":" + el
				}
				out = append(out, el)
			}
			return out
		default:
			return []string{v.String()}
		}
	}
	walk(v, "")
	out := map[string]string{}
	for k, vs := range found {
		sort.Strings(vs)
		out[k] = strings.Join(vs, "\x00")
	}
	return out
}

func (r *recorder) ImportResourceState(ctx context.Context, req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	resp, err := r.server.ImportResourceState(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	var recordErr error
	for _, ir := range resp.ImportedResources {
		if st, ok := r.decode(ctx, ir.TypeName, ir.State); ok {
			recordErr = errors.Join(recordErr, r.remember(ir.TypeName, st["id"], func(m *memory) { m.Stage = "imported" }))
		}
	}
	return failed(resp, err, recordErr)
}

func (r *recorder) ReadResource(ctx context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	var recordErr error
	if st, ok := r.decode(ctx, req.TypeName, req.CurrentState); ok {
		recordErr = r.remember(req.TypeName, st["id"], func(m *memory) {
			if m.Stage == "imported" {
				m.Stage = "read"
			} else {
				m.Stage = ""
			}
		})
	}
	resp, err := r.server.ReadResource(ctx, req)
	return failed(resp, err, recordErr)
}

func (r *recorder) ApplyResourceChange(ctx context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	resp, err := r.server.ApplyResourceChange(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	// A create's plan had no id to remember its configuration under.
	if st, ok := r.decode(ctx, req.TypeName, resp.NewState); ok && st["id"] != "" && st["id"] != "<unknown>" {
		config, _ := r.decode(ctx, req.TypeName, req.Config)
		return failed(resp, err, r.remember(req.TypeName, st["id"], func(m *memory) { m.Config, m.HasConfig = config, true }))
	}
	return resp, err
}

func (r *recorder) PlanResourceChange(ctx context.Context, req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	resp, err := r.server.PlanResourceChange(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	for _, d := range resp.Diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			return resp, err
		}
	}
	prior, hasPrior := r.decode(ctx, req.TypeName, req.PriorState)
	config, _ := r.decode(ctx, req.TypeName, req.Config)
	planned, isPlanned := r.decode(ctx, req.TypeName, resp.PlannedState)
	if !isPlanned {
		return resp, err // a destroy
	}
	if !hasPrior {
		if id := planned["id"]; id != "" && id != "<unknown>" {
			return failed(resp, err, r.remember(req.TypeName, id, func(m *memory) { m.Config, m.HasConfig = config, true }))
		}
		return resp, err
	}
	var last memory
	if recordErr := r.remember(req.TypeName, prior["id"], func(m *memory) {
		last = *m
		m.Stage = ""
		m.Config, m.HasConfig = config, true
	}); recordErr != nil {
		return failed(resp, err, recordErr)
	}
	afterImport := last.Stage == "read"

	writeOnly := map[string]bool{}
	if s := r.schema(ctx, req.TypeName); s != nil && s.Block != nil {
		for _, a := range s.Block.Attributes {
			writeOnly[a.Name] = a.WriteOnly
		}
	}
	var replaced []string
	for _, p := range resp.RequiresReplace {
		replaced = append(replaced, attrPathString(p))
	}
	var imported, updated, replacing []string
	for path := range union(prior, config) {
		// The plan after an import holds what the import read, for what the
		// configuration sets and for what only state has.
		if afterImport && (writeOnly[path] && config[path] != "" || planned[path] == prior[path]) {
			imported = append(imported, path)
		}
	}
	if !afterImport && last.HasConfig {
		for path := range union(prior, planned, config) {
			// The test changed the configuration, and the plan moves the
			// value, in place or by a replacement.
			if config[path] == last.Config[path] || !writeOnly[path] && planned[path] == prior[path] {
				continue
			}
			if coveredBy(path, replaced) {
				replacing = append(replacing, path)
			} else {
				updated = append(updated, path)
			}
		}
	}
	return failed(resp, err, errors.Join(
		r.emit(req.TypeName, CellImport, imported),
		r.emit(req.TypeName, CellUpdate, updated),
		r.emit(req.TypeName, CellReplace, replacing)))
}

func (r *recorder) UpgradeResourceState(ctx context.Context, req *tfprotov6.UpgradeResourceStateRequest) (*tfprotov6.UpgradeResourceStateResponse, error) {
	resp, err := r.server.UpgradeResourceState(ctx, req)
	if s := r.schema(ctx, req.TypeName); err == nil && resp != nil && s != nil && req.Version < s.Version {
		if st, ok := r.decode(ctx, req.TypeName, resp.UpgradedState); ok {
			paths := make([]string, 0, len(st))
			for p := range st {
				paths = append(paths, p)
			}
			return failed(resp, err, r.emit(req.TypeName, CellUpgrade, paths))
		}
	}
	return resp, err
}

func union(ms ...map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		for k := range m {
			out[k] = true
		}
	}
	return out
}

// attrPathString writes an attribute path's names joined by dots, leaving out
// element keys and indexes, the way Flatten folds them.
func attrPathString(p *tftypes.AttributePath) string {
	var names []string
	for _, s := range p.Steps() {
		if n, ok := s.(tftypes.AttributeName); ok {
			names = append(names, string(n))
		}
	}
	return strings.Join(names, ".")
}

func coveredBy(path string, replaced []string) bool {
	for _, r := range replaced {
		if path == r || strings.HasPrefix(path, r+".") {
			return true
		}
	}
	return false
}
