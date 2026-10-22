// Command clientspec cuts the k0rdent API contract down to the operations the
// provider calls, and records where the cut came from.
//
// The source is the prepared contract in k0rdent-clients, which already holds
// only the public operations. This keeps the operations named in
// internal/k0rdent/spec/operations.txt, the path-level fields of their paths,
// the tags they use, and every component they reach through a $ref, at any
// depth. Everything else is dropped, so the client generated from the result
// has no method, type or limit for an operation the provider doesn't call.
//
// It has two modes:
//
//	clientspec -from <k0rdent-clients>/go -commit <sha>   filter, write spec/openapi.yaml and spec/SOURCE.json
//	clientspec -contract                                  write contract.gen.go from spec/SOURCE.json
//
// `make client-sync` runs the first, and `go generate ./internal/k0rdent`
// runs the second, so contract.gen.go is covered by `make client-check`.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// source is spec/SOURCE.json: where the filtered spec came from.
type source struct {
	ClientsRepo    string   `json:"clientsRepo"`
	ClientsCommit  string   `json:"clientsCommit"`
	PreparedSpec   string   `json:"preparedSpec"`
	PreparedSHA256 string   `json:"preparedSHA256"`
	Contract       contract `json:"contract"`
	Operations     int      `json:"operations"`
	Paths          int      `json:"paths"`
	Components     int      `json:"components"`
}

// contract is the k0rdent API release the prepared spec was made from, as
// k0rdent-clients records it in spec/prepared/PREPARE.json.
type contract struct {
	ReleaseTag   string `json:"releaseTag"`
	SourceCommit string `json:"sourceCommit"`
	SHA256       string `json:"sha256"`
}

const (
	preparedSpec = "spec/prepared/openapi.yaml"
	preparedInfo = "spec/prepared/PREPARE.json"
)

func main() {
	var (
		from, commit, opsFile, out, sourceFile, contractOut string
		contractOnly                                        bool
	)
	flag.StringVar(&from, "from", "", "the k0rdent-clients go/ directory")
	flag.StringVar(&commit, "commit", "", "the k0rdent-clients commit -from is checked out at")
	flag.StringVar(&opsFile, "ops", "spec/operations.txt", "the operationIds to keep, one per line")
	flag.StringVar(&out, "out", "spec/openapi.yaml", "the filtered spec")
	flag.StringVar(&sourceFile, "source", "spec/SOURCE.json", "the source record")
	flag.BoolVar(&contractOnly, "contract", false, "only write -contract-out from -source")
	flag.StringVar(&contractOut, "contract-out", "contract.gen.go", "the contract constants")
	flag.Parse()

	var err error
	if contractOnly {
		err = writeContract(sourceFile, contractOut)
	} else {
		err = sync(from, commit, opsFile, out, sourceFile)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "clientspec: %v\n", err)
		os.Exit(1)
	}
}

func sync(from, commit, opsFile, out, sourceFile string) error {
	if from == "" || commit == "" {
		return errors.New("-from and -commit are required")
	}
	raw, err := os.ReadFile(filepath.Join(from, preparedSpec)) //nolint:gosec // a path the caller names
	if err != nil {
		return err
	}
	info, err := os.ReadFile(filepath.Join(from, preparedInfo)) //nolint:gosec // as above
	if err != nil {
		return err
	}
	var prepared struct {
		Source contract `json:"source"`
	}
	if err = json.Unmarshal(info, &prepared); err != nil {
		return fmt.Errorf("%s: %w", preparedInfo, err)
	}
	ops, err := readOps(opsFile)
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", preparedSpec, err)
	}
	stats, err := filter(&doc, ops)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err = enc.Encode(&doc); err != nil {
		return err
	}
	if err = enc.Close(); err != nil {
		return err
	}
	if err = os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
		return err
	}

	sum := sha256.Sum256(raw)
	rec := source{
		ClientsRepo:    "k0rdent/k0rdent-clients",
		ClientsCommit:  commit,
		PreparedSpec:   "go/" + preparedSpec,
		PreparedSHA256: hex.EncodeToString(sum[:]),
		Contract:       prepared.Source,
		Operations:     stats.operations,
		Paths:          stats.paths,
		Components:     stats.components,
	}
	js, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(sourceFile, append(js, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("clientspec: kept %d operations on %d paths and %d components\n",
		stats.operations, stats.paths, stats.components)
	return nil
}

func readOps(file string) (map[string]bool, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // a path the caller names
	if err != nil {
		return nil, err
	}
	ops := map[string]bool{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if ops[line] {
			return nil, fmt.Errorf("%s lists %s twice", file, line)
		}
		ops[line] = true
	}
	return ops, nil
}

type stats struct{ operations, paths, components int }

var methods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

func filter(doc *yaml.Node, ops map[string]bool) (stats, error) {
	var st stats
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return st, errors.New("not a YAML document")
	}
	root := doc.Content[0]

	paths := get(root, "paths")
	if paths == nil {
		return st, errors.New("no paths")
	}
	found := map[string]bool{}
	tags := map[string]bool{}
	security := map[string]bool{}
	collectSecurity(get(root, "security"), security)
	var keptPaths []*yaml.Node
	for i := 0; i+1 < len(paths.Content); i += 2 {
		item := paths.Content[i+1]
		var kept []*yaml.Node
		n := 0
		for j := 0; j+1 < len(item.Content); j += 2 {
			key, val := item.Content[j], item.Content[j+1]
			if !methods[key.Value] {
				kept = append(kept, key, val)
				continue
			}
			id := scalar(get(val, "operationId"))
			if !ops[id] {
				continue
			}
			found[id] = true
			n++
			kept = append(kept, key, val)
			for _, t := range seq(get(val, "tags")) {
				tags[t.Value] = true
			}
			collectSecurity(get(val, "security"), security)
		}
		if n == 0 {
			continue
		}
		item.Content = kept
		keptPaths = append(keptPaths, paths.Content[i], item)
		st.paths++
		st.operations += n
	}
	paths.Content = keptPaths

	var missing []string
	for id := range ops {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return st, fmt.Errorf("not in the prepared spec: %s", strings.Join(missing, ", "))
	}

	filterTags(root, tags)

	components := get(root, "components")
	if components != nil {
		st.components = pruneComponents(components, paths, security)
	}
	return st, nil
}

// pruneComponents keeps each component a kept path reaches through a $ref,
// directly or through other components, and the security schemes the kept
// operations name.
func pruneComponents(components, paths *yaml.Node, security map[string]bool) int {
	reached := map[string]bool{}
	queue := refs(paths, nil)
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		if reached[ref] {
			continue
		}
		reached[ref] = true
		section, name, ok := splitRef(ref)
		if !ok {
			continue
		}
		if node := get(get(components, section), name); node != nil {
			queue = refs(node, queue)
		}
	}

	n := 0
	var keptSections []*yaml.Node
	for i := 0; i+1 < len(components.Content); i += 2 {
		section, entries := components.Content[i].Value, components.Content[i+1]
		var kept []*yaml.Node
		for j := 0; j+1 < len(entries.Content); j += 2 {
			name := entries.Content[j].Value
			keep := reached["#/components/"+section+"/"+name]
			if section == "securitySchemes" {
				keep = security[name]
			}
			if keep {
				kept = append(kept, entries.Content[j], entries.Content[j+1])
				n++
			}
		}
		if len(kept) == 0 {
			continue
		}
		entries.Content = kept
		keptSections = append(keptSections, components.Content[i], entries)
	}
	components.Content = keptSections
	return n
}

// refs appends every component reference under node: each $ref value, and
// each discriminator mapping value, which names a schema without a $ref key.
func refs(node *yaml.Node, out []string) []string {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			if key.Value == "$ref" && val.Kind == yaml.ScalarNode {
				out = append(out, val.Value)
				continue
			}
			if key.Value == "mapping" && val.Kind == yaml.MappingNode {
				for j := 1; j < len(val.Content); j += 2 {
					if strings.HasPrefix(val.Content[j].Value, "#/components/") {
						out = append(out, val.Content[j].Value)
					}
				}
				continue
			}
			out = refs(val, out)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range node.Content {
			out = refs(c, out)
		}
	}
	return out
}

func splitRef(ref string) (section, name string, ok bool) {
	rest, ok := strings.CutPrefix(ref, "#/components/")
	if !ok {
		return "", "", false
	}
	section, name, ok = strings.Cut(rest, "/")
	if !ok || strings.Contains(name, "/") {
		return "", "", false
	}
	name = strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
	return section, name, true
}

func collectSecurity(node *yaml.Node, into map[string]bool) {
	for _, req := range seq(node) {
		for i := 0; i < len(req.Content); i += 2 {
			into[req.Content[i].Value] = true
		}
	}
}

// filterTags drops the tags no kept operation uses, from the top-level list
// and from each x-tagGroups group, and drops a group left empty.
func filterTags(root *yaml.Node, used map[string]bool) {
	if list := get(root, "tags"); list != nil {
		list.Content = slices.DeleteFunc(list.Content, func(t *yaml.Node) bool {
			return !used[scalar(get(t, "name"))]
		})
	}
	if groups := get(root, "x-tagGroups"); groups != nil {
		groups.Content = slices.DeleteFunc(groups.Content, func(g *yaml.Node) bool {
			members := get(g, "tags")
			if members == nil {
				return true
			}
			members.Content = slices.DeleteFunc(members.Content, func(t *yaml.Node) bool {
				return !used[t.Value]
			})
			return len(members.Content) == 0
		})
	}
}

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func seq(n *yaml.Node) []*yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

func scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func writeContract(sourceFile, out string) error {
	raw, err := os.ReadFile(sourceFile) //nolint:gosec // a path the caller names
	if err != nil {
		return err
	}
	var rec source
	if err = json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("%s: %w", sourceFile, err)
	}
	c := rec.Contract
	if c.ReleaseTag == "" || c.SourceCommit == "" || c.SHA256 == "" {
		return fmt.Errorf("%s has no contract release, commit and hash", sourceFile)
	}
	src := fmt.Sprintf(`// Code generated by internal/tools/clientspec from spec/SOURCE.json. DO NOT EDIT.

package k0rdent

// The k0rdent API contract this client is generated from. ContractSHA256 is
// the pin: the hash of the release asset the prepared spec was made from.
const (
	// ContractRelease is the k0rdent API release tag.
	ContractRelease = %q
	// ContractCommit is the commit the release was cut from.
	ContractCommit = %q
	// ContractSHA256 is the SHA-256 of the release's openapi.yaml asset.
	ContractSHA256 = %q
)
`, c.ReleaseTag, c.SourceCommit, c.SHA256)
	formatted, err := format.Source([]byte(src))
	if err != nil {
		return err
	}
	return os.WriteFile(out, formatted, 0o600)
}
