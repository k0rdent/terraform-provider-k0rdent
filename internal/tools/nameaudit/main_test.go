package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanSkipsGeneratedBinaryButAuditsText(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".demo"), 0o755); err != nil {
		t.Fatal(err)
	}

	name := "k0rdent"
	resourceType := name + "_kubernetes_cluster"

	// Model a generated executable containing provider-name strings. Its binary
	// bytes must not become rename findings or stop the audit.
	binary := append([]byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00},
		[]byte("terraform-provider-"+resourceType)...)
	if err := os.WriteFile(filepath.Join(root, ".demo", "terraform-provider-"+name), binary, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "provider.tf"), []byte("resource \""+resourceType+"\" \"demo\" {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := scan(root, name)
	if err != nil {
		t.Fatalf("scan returned an error: %v", err)
	}
	if len(got.config) != 1 {
		t.Fatalf("got %d configuration hits, want exactly the text control hit: %#v", len(got.config), got.config)
	}
	if got.config[0].path != "provider.tf" {
		t.Fatalf("configuration hit came from %q, want provider.tf", got.config[0].path)
	}
}

func TestScanReportsHardCodedProviderNameInGo(t *testing.T) {
	root := t.TempDir()
	name := "k0rdent"
	src := "package example\n\nconst resourceType = \"" + name + "_kubernetes_cluster\"\n"
	if err := os.WriteFile(filepath.Join(root, "resource.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := scan(root, name)
	if err != nil {
		t.Fatalf("scan returned an error: %v", err)
	}
	if len(got.goSource) != 1 {
		t.Fatalf("got %d Go source hits, want 1: %#v", len(got.goSource), got.goSource)
	}
	if got.goSource[0].path != "resource.go" {
		t.Fatalf("Go source hit came from %q, want resource.go", got.goSource[0].path)
	}
}

func TestScanIgnoresNameauditTestFixtures(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "tools", "nameaudit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "k0rdent"
	src := "package main\n\nconst fixture = \"" + name + "_kubernetes_cluster\"\n"
	if err := os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := scan(root, name)
	if err != nil {
		t.Fatalf("scan returned an error: %v", err)
	}
	if len(got.goSource) != 0 {
		t.Fatalf("auditor test fixtures were treated as hard-codes: %#v", got.goSource)
	}
}

func TestScanFlagsHardCodedNameInProviderTests(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "provider")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "k0rdent"
	src := "package provider\n\nconst resourceType = \"" + name + "_kubernetes_cluster\"\n"
	if err := os.WriteFile(filepath.Join(dir, "names_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := scan(root, name)
	if err != nil {
		t.Fatalf("scan returned an error: %v", err)
	}
	if len(got.goSource) != 1 {
		t.Fatalf("got %d Go source hits, want 1 from a provider test: %#v", len(got.goSource), got.goSource)
	}
}
