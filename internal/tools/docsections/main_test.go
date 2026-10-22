package main

import (
	"strings"
	"testing"
)

const section = "## Timeouts\n\nbody\n"

func TestInsertGoesBeforeImport(t *testing.T) {
	page := "# example_x (Resource)\n\nschema\n\n## Import\n\nimport text\n"
	got, err := insert(page, section)
	if err != nil {
		t.Fatal(err)
	}
	want := "# example_x (Resource)\n\nschema\n\n## Timeouts\n\nbody\n\n## Import\n\nimport text\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestInsertAppendsWithoutImport(t *testing.T) {
	got, err := insert("# example_x (Data Source)\n\nschema\n", section)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "schema\n\n## Timeouts\n\nbody\n") {
		t.Fatalf("got\n%s", got)
	}
}

// Running twice without the generator in between would stack a second copy.
func TestInsertRefusesAPageThatAlreadyHasTheSection(t *testing.T) {
	once, err := insert("# example_x\n", section)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insert(once, section); err == nil {
		t.Fatal("a second insert was accepted")
	}
}
