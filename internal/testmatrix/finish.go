package testmatrix

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// fact is what one call saw, for Finish. Import and Read mark those calls.
// Any other fact is a plan or an apply, with its configuration. A plan with
// prior state (Prior) also holds what it can credit: Imported, the paths it
// keeps if it follows an import, and Moved, the paths whose value it moves.
// Replacing says it replaces the object, because a path Replaces covers
// moves, and then it moves nothing in place.
type fact struct {
	Instance  string            `json:"instance"`
	Started   int64             `json:"started"`
	Dir       string            `json:"dir"`
	Test      string            `json:"test"`
	Resource  string            `json:"resource"`
	Object    string            `json:"object"`
	Import    bool              `json:"import,omitempty"`
	Read      bool              `json:"read,omitempty"`
	Config    map[string]string `json:"config,omitempty"`
	Prior     bool              `json:"prior,omitempty"`
	Imported  []string          `json:"imported,omitempty"`
	Moved     []string          `json:"moved,omitempty"`
	Replaces  []string          `json:"replaces,omitempty"`
	Replacing bool              `json:"replacing,omitempty"`
}

func factsDir(file string) string { return file + ".facts" }

// touch is what one provider did to one object.
type touch struct {
	started        int64
	dir            string
	imported, read bool
	configs        []map[string]string
	plans          []fact
	afterImport    bool
}

// Finish turns the facts every provider wrote into the record's import,
// update and replace lines. Run it once the suite is done. complete says the
// whole suite ran and passed, and only then, with no test skipped, does the
// record end with a CellFinished line, which the check needs. A test that
// skipped gets a CellSkipped line instead. For each object, over the
// providers that touched it in the order they started:
//
//   - A provider follows an import when it made the import, or the one
//     before it in the same working directory did and this one doesn't read
//     the object again (`terraform import`, then a plan with -refresh=false). Its plans credit the import
//     cells every one of them keeps: in plugin-testing's generate step it
//     also plans the copy it keeps in state, and the two can't be told apart.
//   - Any other provider credits update or replace cells for what its plans
//     move, where the configuration differs from every one the object had in
//     the same working directory, back to the last provider there that
//     didn't follow an import. A plan that replaces the object credits
//     replace for the paths that make it replace and nothing else, since it
//     updates nothing in place.
func Finish(file string, complete bool) error {
	type key struct{ test, resource, object string }
	objects := map[key]map[string]*touch{}
	facts, err := readFacts(file)
	if err != nil {
		return err
	}
	keptMu.Lock()
	skipped := skips[file]
	delete(skips, file)
	keptMu.Unlock()
	for _, f := range facts {
		k := key{f.Test, f.Resource, f.Object}
		if objects[k] == nil {
			objects[k] = map[string]*touch{}
		}
		t := objects[k][f.Instance]
		if t == nil {
			t = &touch{started: f.Started, dir: f.Dir}
			objects[k][f.Instance] = t
		}
		switch {
		case f.Import:
			t.imported = true
		case f.Read:
			t.read = true
		default:
			if !slices.ContainsFunc(t.configs, func(c map[string]string) bool { return maps.Equal(c, f.Config) }) {
				t.configs = append(t.configs, f.Config)
			}
			if f.Prior {
				t.plans = append(t.plans, f)
			}
		}
	}

	lines := map[Line]bool{}
	for k, byInstance := range objects {
		add := func(cell string, paths ...string) {
			for _, p := range paths {
				lines[Line{Test: k.test, Resource: k.resource, Path: p, Cell: cell}] = true
			}
		}
		touches := slices.SortedFunc(maps.Values(byInstance), func(a, b *touch) int { return cmp.Compare(a.started, b.started) })
		for i, t := range touches {
			prev := lastIn(touches[:i], t.dir)
			t.afterImport = t.imported || prev != nil && prev.imported && !t.read
			if t.afterImport {
				add(CellImport, keptByAll(t.plans)...)
				continue
			}
			history := historyBefore(touches[:i], t.dir)
			if len(history) == 0 {
				continue
			}
			for _, f := range t.plans {
				for _, path := range f.Moved {
					if slices.ContainsFunc(history, func(c map[string]string) bool { return c[path] == f.Config[path] }) {
						continue
					}
					switch {
					case !f.Replacing:
						add(CellUpdate, path)
					case coveredBy(path, f.Replaces):
						add(CellReplace, path)
					}
				}
			}
		}
	}
	sorted := slices.SortedFunc(maps.Keys(lines), func(a, b Line) int {
		return strings.Compare(a.Test+"\x00"+a.Resource+"\x00"+a.Path+"\x00"+a.Cell, b.Test+"\x00"+b.Resource+"\x00"+b.Path+"\x00"+b.Cell)
	})
	values := make([]any, 0, len(sorted)+len(skipped)+1)
	for _, l := range sorted {
		values = append(values, l)
	}
	slices.SortFunc(skipped, func(a, b Line) int { return cmp.Or(cmp.Compare(a.Test, b.Test), cmp.Compare(a.Why, b.Why)) })
	for _, l := range slices.Compact(skipped) {
		values = append(values, l)
	}
	if complete && len(skipped) == 0 {
		values = append(values, Line{Cell: CellFinished})
	}
	return appendJSON(file, values)
}

// lastIn is the last of earlier in dir, or nil.
func lastIn(earlier []*touch, dir string) *touch {
	for i := len(earlier) - 1; i >= 0; i-- {
		if earlier[i].dir == dir {
			return earlier[i]
		}
	}
	return nil
}

// readFacts is the facts this process kept for file, then the ones built
// binaries wrote beside it.
func readFacts(file string) ([]fact, error) {
	keptMu.Lock()
	facts := kept[file]
	delete(kept, file)
	keptMu.Unlock()
	entries, err := os.ReadDir(factsDir(file))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		name := filepath.Join(factsDir(file), e.Name())
		b, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		for dec.More() {
			var f fact
			if err := dec.Decode(&f); err != nil {
				return nil, fmt.Errorf("reading %s: %w", name, err)
			}
			facts = append(facts, f)
		}
	}
	return facts, nil
}

// keptByAll is the import paths every plan keeps.
func keptByAll(plans []fact) []string {
	if len(plans) == 0 {
		return nil
	}
	kept := slices.Clone(plans[0].Imported)
	for _, f := range plans[1:] {
		kept = slices.DeleteFunc(kept, func(p string) bool { return !slices.Contains(f.Imported, p) })
	}
	return kept
}

// historyBefore is the configurations the object had in dir before: the
// last provider there that didn't follow an import, and every one after it.
func historyBefore(earlier []*touch, dir string) []map[string]string {
	var out []map[string]string
	for i := len(earlier) - 1; i >= 0; i-- {
		t := earlier[i]
		if t.dir != dir || len(t.configs) == 0 {
			continue
		}
		out = append(out, t.configs...)
		if !t.afterImport {
			break
		}
	}
	return out
}
