// Package testmatrix records which cells of the testing matrix the acceptance
// tests exercise, from what the provider is asked to do rather than from
// anything a test declares.
//
// With a record file named, every provider server is wrapped in a recorder
// that writes down each import, read, plan and apply it serves, by provider
// and object. Finish turns that into the record's cells once the suite is
// done (finish.go). Terraform makes the calls of one command in parallel, so
// no cell may hang on their order, and only Finish sees them all.
//
// The acceptance tests wrap the servers they start in process
// (internal/acceptance/matrix_record_test.go). A test that runs a built
// provider binary sets RecordEnv and TestEnv in its environment, and main.go
// serves through Serve. internal/repocheck turns the record into
// docs/testing-matrix.md.
package testmatrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// The cells a line can name. CellFinished isn't a cell: Finish ends the
// record with it, and the check refuses a record without it.
const (
	CellImport   = "import"
	CellUpdate   = "update"
	CellReplace  = "replace"
	CellUpgrade  = "upgrade"
	CellFinished = "finished"
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
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	started := time.Now().UnixNano()
	return &recorder{
		server:   full,
		test:     test,
		file:     file,
		started:  started,
		dir:      dir,
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

// recorder is one provider: a Terraform command, or a phase of one.
type recorder struct {
	server
	test, file string
	instance   string
	started    int64
	// dir is the provider's working directory. A built binary runs in the
	// directory Terraform does. In process, every provider shares the test's.
	dir string

	mu         sync.Mutex
	loaded     bool
	schemas    map[string]*tfprotov6.Schema
	identities map[string][]string
}

// recordMu keeps the recorders in one process from interleaving lines.
var recordMu sync.Mutex

// object names the object a state of typ belongs to: the values the state
// holds for the resource's identity attributes. An id isn't enough, since
// the VPC security groups binding has none. ok is false while a value is
// unknown, as in the plan of a create whose id the platform picks.
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

// note writes f down for Finish, for the object st belongs to. A state whose
// object isn't known yet is left out, unless the call needs it.
func (r *recorder) note(ctx context.Context, typ string, st map[string]string, f fact, need bool) error {
	object, ok, err := r.object(ctx, typ, st)
	if err == nil && !ok && need {
		err = fmt.Errorf("can't tell which %s this is", typ)
	}
	if err != nil || !ok {
		return err
	}
	f.Instance, f.Started, f.Dir, f.Test, f.Resource, f.Object = r.instance, r.started, r.dir, r.test, typ, object
	return appendJSON(filepath.Join(factsDir(r.file), r.instance+".jsonl"), []any{f})
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
			recordErr = errors.Join(recordErr, r.note(ctx, ir.TypeName, st, fact{Import: true}, true))
		}
	}
	return failed(resp, err, recordErr)
}

func (r *recorder) ReadResource(ctx context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	resp, err := r.server.ReadResource(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	if st, ok := r.decode(ctx, req.TypeName, req.CurrentState); ok {
		return failed(resp, err, r.note(ctx, req.TypeName, st, fact{Read: true}, false))
	}
	return resp, err
}

// ApplyResourceChange notes the configuration, for a create whose plan
// didn't know the object yet.
func (r *recorder) ApplyResourceChange(ctx context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	resp, err := r.server.ApplyResourceChange(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	if st, ok := r.decode(ctx, req.TypeName, resp.NewState); ok {
		config, _ := r.decode(ctx, req.TypeName, req.Config)
		return failed(resp, err, r.note(ctx, req.TypeName, st, fact{Config: config}, false))
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
		return failed(resp, err, r.note(ctx, req.TypeName, planned, fact{Config: config}, false))
	}
	writeOnly := map[string]bool{}
	if s := r.schema(ctx, req.TypeName); s != nil && s.Block != nil {
		for _, a := range s.Block.Attributes {
			writeOnly[a.Name] = a.WriteOnly
		}
	}
	f := fact{Config: config, Prior: true}
	for _, p := range resp.RequiresReplace {
		f.Replaces = append(f.Replaces, attrPathString(p))
	}
	for path := range union(prior, config) {
		// What a plan after an import keeps of it.
		if writeOnly[path] && config[path] != "" || planned[path] == prior[path] {
			f.Imported = append(f.Imported, path)
		}
	}
	for path := range union(prior, planned, config) {
		// What it moves. A write-only value moves whenever it's set.
		if writeOnly[path] || planned[path] != prior[path] {
			f.Moved = append(f.Moved, path)
		}
	}
	sort.Strings(f.Imported)
	sort.Strings(f.Moved)
	return failed(resp, err, r.note(ctx, req.TypeName, prior, f, true))
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
