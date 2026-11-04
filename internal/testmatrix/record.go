// Package testmatrix records which cells of the testing matrix the acceptance
// tests exercise, from what the provider is asked to do rather than from
// anything a test declares.
//
// With a record file named, every provider server is wrapped in a recorder
// that watches the RPCs Terraform makes, and the record gets a line for each
// cell a test exercised:
//
//   - import: a plan of an object by the provider that imported it, or by
//     the next one when that one doesn't read it again (`terraform import`,
//     then `terraform plan -refresh=false`), for each attribute the
//     configuration sets or state holds, where the plan keeps the imported
//     value. A write-only attribute counts when the configuration sets it.
//   - update: a plan with prior state and no error, by a provider that didn't
//     import the object, where the test changed the attribute's
//     configuration from every one the object was last planned with, the
//     planned value differs from the prior one (for a write-only attribute,
//     the configuration change is enough), and no RequiresReplace path
//     covers it.
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
// Each Terraform command starts a new provider, so the configurations an
// object was last planned with are kept in files beside the record, by test,
// type and object. An object is the values its state holds for the
// resource's identity attributes, the ones an import block's identity names,
// since not every resource has an id.
//
// One provider can plan the same object twice, under two addresses: the
// step of terraform-plugin-testing that generates configuration keeps the
// resource in state and imports it again beside it, and both read back the
// same state. Terraform makes those calls in parallel, in no fixed order, so
// a plan can't tell whether the import it belongs with has happened yet.
// The recorder writes down what each import, read and plan saw, by
// provider, and Finish works out the cells once every provider is done, so
// the record never depends on the order of the calls. A configuration counts as changed
// against the ones the provider before this one planned, for the same reason.
package testmatrix

import (
	"bytes"
	"cmp"
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
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	started := time.Now().UnixNano()
	return &recorder{
		server:   full,
		test:     test,
		file:     file,
		started:  started,
		instance: fmt.Sprintf("%d-%d-%d", os.Getpid(), started, instances.Add(1)),
	}, nil
}

// instances numbers the recorders a process makes, for their names.
var instances atomic.Int64

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
	// instance names this provider, a Terraform command or a phase of one,
	// apart from every other the run starts, and started is when it did.
	instance string
	started  int64

	mu         sync.Mutex
	loaded     bool
	schemas    map[string]*tfprotov6.Schema
	identities map[string][]string
}

// memory is the configurations one object was planned or applied with, by
// the provider Instance names and by the one before it.
type memory struct {
	Instance string              `json:"instance,omitempty"`
	Previous []map[string]string `json:"previous,omitempty"`
	Current  []map[string]string `json:"current,omitempty"`
}

// plannedWith adds config to what instance planned the object with, and
// gives what the provider before it did. Whichever call from a new instance
// comes first moves the last one's configurations to Previous, so every
// call from one instance gets the same answer.
func (m *memory) plannedWith(instance string, config map[string]string) (previous []map[string]string) {
	if m.Instance != instance {
		if m.Instance != "" {
			m.Previous = m.Current
		}
		m.Instance, m.Current = instance, nil
	}
	if !slices.ContainsFunc(m.Current, func(c map[string]string) bool { return maps.Equal(c, config) }) {
		m.Current = append(m.Current, config)
	}
	return m.Previous
}

// The recorders in one process share these, so two servers a test holds at
// once don't race on a memory file or a record line.
var (
	memoryMu sync.Mutex
	recordMu sync.Mutex
)

func (r *recorder) memoryFile(typ, object string) string {
	sum := sha256.Sum256([]byte(r.test + "\x00" + typ + "\x00" + object))
	return filepath.Join(r.file+".memory", hex.EncodeToString(sum[:])+".json")
}

// object names the object a state of typ belongs to: the values the state
// holds for the resource's identity attributes. An id isn't enough: the
// VPC security groups binding has none, so by id every binding a test made
// would be one object. ok is false while one of the values is unknown, as in
// the plan of a create whose id the platform picks.
func (r *recorder) object(ctx context.Context, typ string, st map[string]string) (object string, ok bool, err error) {
	names := r.identity(ctx, typ)
	if len(names) == 0 {
		return "", false, fmt.Errorf("%s has no identity schema, so the recorder can't tell its objects apart", typ)
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if st[n] == "<unknown>" {
			return "", false, nil
		}
		parts = append(parts, n+"="+st[n])
	}
	return strings.Join(parts, "\x00"), true, nil
}

// rememberState is remember for the object st belongs to. A state whose
// object isn't known yet is left alone.
func (r *recorder) rememberState(ctx context.Context, typ string, st map[string]string, change func(*memory)) error {
	object, ok, err := r.object(ctx, typ, st)
	if err != nil || !ok {
		return err
	}
	return r.remember(typ, object, change)
}

// remember loads an object's memory, lets change edit it, and saves it.
func (r *recorder) remember(typ, object string, change func(*memory)) error {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	file := r.memoryFile(typ, object)
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
	change(&m)
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
	sort.Strings(paths)
	lines := make([]any, 0, len(paths))
	for _, p := range paths {
		lines = append(lines, Line{Test: r.test, Resource: typ, Path: p, Cell: cell})
	}
	return appendJSON(r.file, lines)
}

// appendJSON appends one JSON line for each of values to file.
func appendJSON(file string, values []any) error {
	if len(values) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, v := range values {
		if err := enc.Encode(v); err != nil {
			return err
		}
	}
	recordMu.Lock()
	defer recordMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
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

// fact is what one import, read or plan saw, for Finish. A plan's fact
// holds the cells it exercised both ways: Imported if it follows an import,
// Updated and Replaced if it doesn't.
type fact struct {
	Instance string   `json:"instance"`
	Started  int64    `json:"started"`
	Test     string   `json:"test"`
	Resource string   `json:"resource"`
	Object   string   `json:"object"`
	Import   bool     `json:"import,omitempty"`
	Read     bool     `json:"read,omitempty"`
	Imported []string `json:"imported,omitempty"`
	Updated  []string `json:"updated,omitempty"`
	Replaced []string `json:"replaced,omitempty"`
}

func factsDir(file string) string { return file + ".facts" }

func (r *recorder) note(f fact) error {
	f.Instance, f.Started, f.Test = r.instance, r.started, r.test
	return appendJSON(filepath.Join(factsDir(r.file), r.instance+".jsonl"), []any{f})
}

// Finish turns what every provider of the run saw into the record's import,
// update and replace lines. Run it once they've all stopped, before the
// record is read.
func Finish(file string) error {
	entries, err := os.ReadDir(factsDir(file))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// What each provider did to each object, by test.
	type object struct{ test, resource, object string }
	type provider struct {
		started        int64
		imported, read bool
		plans          []fact
	}
	objects := map[object]map[string]*provider{}
	for _, e := range entries {
		name := filepath.Join(factsDir(file), e.Name())
		b, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		for dec.More() {
			var f fact
			if err := dec.Decode(&f); err != nil {
				return fmt.Errorf("reading %s: %w", name, err)
			}
			o := object{f.Test, f.Resource, f.Object}
			if objects[o] == nil {
				objects[o] = map[string]*provider{}
			}
			p := objects[o][f.Instance]
			if p == nil {
				p = &provider{started: f.Started}
				objects[o][f.Instance] = p
			}
			switch {
			case f.Import:
				p.imported = true
			case f.Read:
				p.read = true
			default:
				p.plans = append(p.plans, f)
			}
		}
	}
	seen := map[Line]bool{}
	var lines []Line
	add := func(f fact, cell string, paths []string) {
		for _, p := range paths {
			l := Line{Test: f.Test, Resource: f.Resource, Path: p, Cell: cell}
			if !seen[l] {
				seen[l] = true
				lines = append(lines, l)
			}
		}
	}
	for _, byInstance := range objects {
		// One test's commands run one after another, so the providers that
		// touched an object, in the order they started, are its history.
		providers := slices.SortedFunc(maps.Values(byInstance), func(a, b *provider) int { return cmp.Compare(a.started, b.started) })
		for i, p := range providers {
			afterImport := p.imported || i > 0 && providers[i-1].imported && !p.read
			for _, f := range p.plans {
				if afterImport {
					add(f, CellImport, f.Imported)
				} else {
					add(f, CellUpdate, f.Updated)
					add(f, CellReplace, f.Replaced)
				}
			}
		}
	}
	sort.Slice(lines, func(i, j int) bool {
		a, b := lines[i], lines[j]
		return a.Test+"\x00"+a.Resource+"\x00"+a.Path+"\x00"+a.Cell < b.Test+"\x00"+b.Resource+"\x00"+b.Path+"\x00"+b.Cell
	})
	values := make([]any, len(lines))
	for i, l := range lines {
		values[i] = l
	}
	return appendJSON(file, values)
}

// failed is the RPC's result with the recorder's own error added, so a cell
// the recorder couldn't write fails the test rather than leaving a gap.
func failed[T any](resp T, err, recordErr error) (T, error) {
	if recordErr != nil {
		return resp, errors.Join(err, fmt.Errorf("testing matrix recorder: %w", recordErr))
	}
	return resp, err
}

// load asks the provider for its resource schemas and identity schemas, once.
func (r *recorder) load(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded {
		return
	}
	resp, err := r.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil || resp == nil {
		return
	}
	ids, err := r.GetResourceIdentitySchemas(ctx, &tfprotov6.GetResourceIdentitySchemasRequest{})
	if err != nil || ids == nil {
		return
	}
	r.schemas = resp.ResourceSchemas
	r.identities = map[string][]string{}
	for typ, s := range ids.IdentitySchemas {
		for _, a := range s.IdentityAttributes {
			r.identities[typ] = append(r.identities[typ], a.Name)
		}
		sort.Strings(r.identities[typ])
	}
	r.loaded = true
}

func (r *recorder) schema(ctx context.Context, typ string) *tfprotov6.Schema {
	r.load(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.schemas[typ]
}

// identity is the names of typ's identity attributes, sorted.
func (r *recorder) identity(ctx context.Context, typ string) []string {
	r.load(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.identities[typ]
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
			object, known, objErr := r.object(ctx, ir.TypeName, st)
			if objErr == nil && !known {
				objErr = fmt.Errorf("an imported %s holds an unknown identity value", ir.TypeName)
			}
			if objErr == nil {
				objErr = r.note(fact{Resource: ir.TypeName, Object: object, Import: true})
			}
			recordErr = errors.Join(recordErr, objErr)
		}
	}
	return failed(resp, err, recordErr)
}

// ReadResource notes the read, which ends what an import in an earlier
// provider means for this one's plans.
func (r *recorder) ReadResource(ctx context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	resp, err := r.server.ReadResource(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	if st, ok := r.decode(ctx, req.TypeName, req.CurrentState); ok {
		object, known, objErr := r.object(ctx, req.TypeName, st)
		if objErr == nil && known {
			objErr = r.note(fact{Resource: req.TypeName, Object: object, Read: true})
		}
		return failed(resp, err, objErr)
	}
	return resp, err
}

func (r *recorder) ApplyResourceChange(ctx context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	resp, err := r.server.ApplyResourceChange(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	// A create's plan may not have known which object to remember its
	// configuration under.
	if st, ok := r.decode(ctx, req.TypeName, resp.NewState); ok {
		config, _ := r.decode(ctx, req.TypeName, req.Config)
		return failed(resp, err, r.rememberState(ctx, req.TypeName, st, func(m *memory) { m.plannedWith(r.instance, config) }))
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
		return failed(resp, err, r.rememberState(ctx, req.TypeName, planned, func(m *memory) { m.plannedWith(r.instance, config) }))
	}
	object, ok, recordErr := r.object(ctx, req.TypeName, prior)
	if recordErr == nil && !ok {
		recordErr = fmt.Errorf("the prior state of a %s holds an unknown identity value", req.TypeName)
	}
	if recordErr != nil {
		return failed(resp, err, recordErr)
	}
	var previous []map[string]string
	if recordErr := r.remember(req.TypeName, object, func(m *memory) { previous = m.plannedWith(r.instance, config) }); recordErr != nil {
		return failed(resp, err, recordErr)
	}

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
	// Both ways, for Finish to pick from once it knows whether this
	// provider imported the object.
	var imported, updated, replacing []string
	for path := range union(prior, config) {
		// The plan after an import holds what the import read, for what the
		// configuration sets and for what only state has.
		if writeOnly[path] && config[path] != "" || planned[path] == prior[path] {
			imported = append(imported, path)
		}
	}
	if len(previous) > 0 {
		for path := range union(prior, planned, config) {
			// The test changed the configuration, from every one the object
			// was last planned with, and the plan moves the value, in place
			// or by a replacement.
			if slices.ContainsFunc(previous, func(c map[string]string) bool { return c[path] == config[path] }) ||
				!writeOnly[path] && planned[path] == prior[path] {
				continue
			}
			if coveredBy(path, replaced) {
				replacing = append(replacing, path)
			} else {
				updated = append(updated, path)
			}
		}
	}
	if len(imported)+len(updated)+len(replacing) == 0 {
		return resp, err
	}
	sort.Strings(imported)
	sort.Strings(updated)
	sort.Strings(replacing)
	return failed(resp, err, r.note(fact{
		Resource: req.TypeName, Object: object,
		Imported: imported, Updated: updated, Replaced: replacing,
	}))
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
