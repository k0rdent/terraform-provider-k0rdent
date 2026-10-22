// Command docsections writes the sections the documentation generator has no
// place for into the pages it generated: Timeouts, with each operation's
// default, and Related, linking the pages a reader of this one wants next.
//
// Both come from provider.PageSections, which reads them from the registered
// schemas and the code that applies the timeouts. scripts/generate-provider-docs.sh
// runs this straight after tfplugindocs, so `make docs` writes them and
// `make docs-check` fails when they go stale.
//
// Each section goes in before the page's Import section, where the AWS and
// Google pages put Timeouts, or at the end of a page with none.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

func main() {
	dir := flag.String("docs", "docs", "the generated documentation directory")
	flag.Parse()
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "docsections:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	sections := provider.PageSections(context.Background())
	pages := make([]string, 0, len(sections))
	for page := range sections {
		pages = append(pages, page)
	}
	slices.Sort(pages)
	for _, page := range pages {
		path := filepath.Join(dir, filepath.FromSlash(page)+".md")
		raw, err := os.ReadFile(path) //nolint:gosec // a page this repository generates
		if err != nil {
			return fmt.Errorf("%s has no generated page: %w", page, err)
		}
		out, err := insert(string(raw), sections[page])
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil { //nolint:gosec // documentation, read by everyone
			return err
		}
	}
	return nil
}

// importHeading is where the generator's Import section starts.
const importHeading = "\n## Import\n"

// insert puts the sections before the page's Import section, or at its end.
// It refuses a page that already carries one of them, which would mean the
// generator did not rewrite it and this ran twice.
func insert(page, sections string) (string, error) {
	if strings.TrimSpace(sections) == "" {
		return page, nil
	}
	for _, line := range strings.Split(sections, "\n") {
		if strings.HasPrefix(line, "## ") && strings.Contains(page, "\n"+line+"\n") {
			return "", fmt.Errorf("already has %q: run the generator first", line)
		}
	}
	if before, after, ok := strings.Cut(page, importHeading); ok {
		return strings.TrimRight(before, "\n") + "\n\n" + strings.TrimRight(sections, "\n") + "\n" + importHeading + after, nil
	}
	return strings.TrimRight(page, "\n") + "\n\n" + strings.TrimRight(sections, "\n") + "\n", nil
}
