// Command nameaudit prints every non-Go place the provider name is written
// down, and fails when the Makefile has drifted from the Go constants.
//
// Go code derives the provider name from one constant. The non-Go half cannot:
// the Makefile, README, examples and the registry manifest each spell it out,
// so this command makes that half enumerable instead of a grep someone has to
// remember to run.
//
// Run it with `make name-audit`. It is part of `make check`, so a name written
// into a new file shows up in CI rather than at rename time.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// skipDirs are not ours to audit.
//
// The run-output directories are skipped because a rename never has to edit a
// run artifact, and because this tool prints each hit's full text: every line
// of .demo's fake log matches on its `ua=` field, and a minified
// .live/terraform.tfstate describes real resources.
var skipDirs = map[string]bool{
	".git":         true,
	".terraform":   true,
	".demo":        true,
	".conformance": true,
	".live":        true,
	".tools":       true,
	"vendor":       true,
	"dist":         true,
}

// clientDir is the generated API client. It's named after the product, not
// the provider, so a rename doesn't touch it, and its spec and templates spell
// the product name throughout.
var clientDir = filepath.Join("internal", "k0rdent")

// docs/ records the naming decision and the KCM collision in prose. Those
// mentions are the subject matter, not configuration, so they are reported
// separately rather than counted as places a rename must touch.
const docsDir = "docs"

// clientImportPath is the generated API client.
//
// Its Go package name is currently spelled exactly like the provisional
// provider type name, and it is not the same thing: the client is named after
// the product and a provider rename does not touch it.
// Without this, every line that calls the client reads as a place a rename
// must edit, which buries the handful of lines that really are.
const clientImportPath = "github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
const providerImportPath = "github.com/k0rdent/terraform-provider-k0rdent"

type hit struct {
	path string
	line int
	text string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "name-audit:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}

	name := provider.ProviderTypeName
	namespace := provider.RegistryNamespace

	fmt.Printf("Provider type name             : %s\n", name)
	fmt.Printf("Registry namespace             : %s\n", namespace)
	fmt.Printf("Registry address               : %s\n\n", provider.RegistryAddress())

	if err = checkMakefile(root, name, namespace); err != nil {
		return err
	}

	found, err := scan(root, name)
	if err != nil {
		return err
	}

	fmt.Println("Places a rename must edit:")
	if len(found.config) == 0 {
		fmt.Println("  (none outside internal/provider/names.go)")
	}
	for _, h := range found.config {
		fmt.Printf("  %s:%d  %s\n", h.path, h.line, h.text)
	}

	fmt.Printf("\nProse mentions in %s/ (read, do not blind-replace): %d\n", docsDir, len(found.docs))
	fmt.Printf("Uses of the %s client package, which a rename does not touch: %d\n",
		path.Base(clientImportPath), len(found.client))

	// Go can always derive the name, so a Go file that spells it out is a
	// mistake rather than a chore, and this is the gate that says so at the
	// time it is written instead of at rename time.
	if len(found.goSource) > 0 {
		fmt.Fprintln(os.Stderr, "\nThe provider name is written out in Go source. Derive it from the\n"+
			"constants in internal/provider/names.go instead: ProviderTypeName,\n"+
			"ResourceName, EnvVar, UserAgent:")
		for _, h := range found.goSource {
			fmt.Fprintf(os.Stderr, "  %s:%d  %s\n", h.path, h.line, h.text)
		}
		return fmt.Errorf("%d Go source line(s) hard-code the provider name", len(found.goSource))
	}

	return nil
}

func repoRoot() (string, error) {
	// `go run ./internal/tools/nameaudit` executes with the module root as the
	// working directory, which is what make invokes.
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(wd, "go.mod")); err != nil {
		return "", fmt.Errorf("run from the module root: %w", err)
	}
	return wd, nil
}

// checkMakefile is the drift gate. The Makefile cannot import a Go constant,
// so it restates the name; this fails the build when the two disagree.
func checkMakefile(root, name, namespace string) error {
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return fmt.Errorf("read Makefile: %w", err)
	}

	for _, want := range []struct{ variable, value string }{
		{"PROVIDER_NAME", name},
		{"NAMESPACE", namespace},
	} {
		got, ok := makeVar(string(data), want.variable)
		if !ok {
			return fmt.Errorf("the Makefile has no %s variable", want.variable)
		}
		if got != want.value {
			return fmt.Errorf("the Makefile sets %s to %q but the Go constant is %q; they must match", want.variable, got, want.value)
		}
	}
	return nil
}

func makeVar(makefile, variable string) (string, bool) {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(variable) + `\s*\??=\s*(\S+)`)
	m := re.FindStringSubmatch(makefile)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// results splits every mention of the name by what a rename would have to do
// about it.
type results struct {
	// config is the non-Go, non-docs places a rename must edit by hand.
	config []hit
	// docs is prose that describes the naming problem itself.
	docs []hit
	// client is Go that names the generated client package, which is a
	// different thing that happens to share the spelling.
	client []hit
	// goSource is Go that writes the provider's own name out. This is the failure
	// case, except for module import paths.
	goSource []hit
}

func scan(root, name string) (results, error) {
	var out results

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || rel == clientDir {
				return fs.SkipDir
			}
			return nil
		}
		// names.go is the definition. Reporting it would be circular.
		if rel == filepath.Join("internal", "provider", "names.go") {
			return nil
		}
		found, err := grepFile(p, rel, name)
		if err != nil {
			return err
		}

		switch {
		case strings.HasPrefix(rel, docsDir+string(filepath.Separator)):
			out.docs = append(out.docs, found...)
		case filepath.Ext(rel) == ".go":
			// The auditor's own tests feed it fixture strings that look like
			// hard-coded Terraform type names. Those are inputs to scan(), not
			// production hard-codes. Provider tests outside this package still
			// fail the gate.
			if isNameauditTest(rel) {
				break
			}
			for _, h := range found {
				if !isProviderTypeLiteral(h.text, name) {
					continue
				}
				if isClientUse(h.text, name) {
					out.client = append(out.client, h)
					continue
				}
				out.goSource = append(out.goSource, h)
			}
		default:
			out.config = append(out.config, found...)
		}
		return nil
	})
	if walkErr != nil {
		return results{}, walkErr
	}

	for _, bucket := range [][]hit{out.config, out.client, out.goSource} {
		sortHits(bucket)
	}
	return out, nil
}

// isClientUse reports whether every mention of name on this line is inside a
// module import path or generated client qualifier. Strip those and see whether
// anything is left. This matters now that the provider type is k0rdent: the
// repository's own Go module path necessarily contains that word.
func isClientUse(text, name string) bool {
	stripped := strings.ReplaceAll(text, clientImportPath, "")
	stripped = strings.ReplaceAll(stripped, path.Base(clientImportPath)+".", "")
	stripped = strings.ReplaceAll(stripped, providerImportPath, "")
	return !strings.Contains(strings.ToLower(stripped), strings.ToLower(name))
}

func isNameauditTest(rel string) bool {
	dir, file := filepath.Split(rel)
	return filepath.Clean(dir) == filepath.Join("internal", "tools", "nameaudit") &&
		strings.HasSuffix(file, "_test.go")
}

// isProviderTypeLiteral distinguishes the provider's public spelling from the
// product name in ordinary Go prose and API hostnames. With `k0rdent` as the
// provider name, a case-insensitive substring scan would otherwise reject
// unavoidable references such as api.k0rdent.ai. The audit's job is to prevent
// hard-coded Terraform type names and environment prefixes.
func isProviderTypeLiteral(text, name string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, strings.ToLower(name)+"_") ||
		strings.Contains(strings.ToUpper(text), strings.ToUpper(name)+"_")
}

func sortHits(hits []hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].path == hits[j].path {
			return hits[i].line < hits[j].line
		}
		return hits[i].path < hits[j].path
	})
}

func grepFile(filename, rel, name string) ([]hit, error) {
	f, err := os.Open(filename) //nolint:gosec // paths come from walking the repo itself
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	// Generated executables and archives can sit below ignored build directories
	// such as .demo/. They are not text configuration and must not be scanned as
	// source. Detect binary content before the line scanner so a long binary
	// token cannot hide the rest of the audit. Repository text files are UTF-8.
	prefix := make([]byte, 8192)
	n, readErr := f.Read(prefix)
	if readErr != nil && readErr != io.EOF {
		return nil, readErr
	}
	prefix = prefix[:n]
	if bytes.IndexByte(prefix, 0) >= 0 || !utf8.Valid(prefix) {
		return nil, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}

	// Case-insensitive on purpose: environment variables spell the name in
	// upper case, and those have to be renamed too.
	needle := strings.ToLower(name)

	var hits []hit
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; scanner.Scan(); n++ {
		text := scanner.Text()
		// Generated binaries can contain the provider name and arbitrary bytes.
		// They are not source or configuration, and printing them breaks callers
		// that consume this audit as UTF-8 text (notably macOS awk).
		if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
			return nil, nil
		}
		if !strings.Contains(strings.ToLower(text), needle) {
			continue
		}
		hits = append(hits, hit{path: rel, line: n, text: strings.TrimSpace(text)})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}
