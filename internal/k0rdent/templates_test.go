package k0rdent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The operations table keys responses by int, so a "default" or "4XX" key
// rendered as written is not Go, and the generator's gofmt pass rejects the
// whole file. No operation in the contract has one today, which is why the
// drift gate can't see it. This generates a client for a spec that does,
// through the templates oapi-codegen.client.yaml names.
func TestOperationsTableSkipsResponsesWithNoExactStatus(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "openapi.yaml")
	if err := os.WriteFile(spec, []byte(`openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /things:
    get:
      operationId: listThings
      responses:
        '200': {description: The things.}
        '4XX': {description: A client error.}
        default: {description: Anything else.}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile("oapi-codegen.client.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "client.gen.go")
	cfg["output"] = out
	templates := cfg["output-options"].(map[string]any)["user-templates"].(map[string]any)
	for name, path := range templates {
		abs, err := filepath.Abs(path.(string))
		if err != nil {
			t.Fatal(err)
		}
		templates[name] = abs
	}
	edited, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	if msg, err := exec.Command("go", "tool", "oapi-codegen", "-config", config, spec).CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, msg)
	}
	gen, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	_, entry, ok := strings.Cut(string(gen), `"listThings": {`)
	entry, _, ok2 := strings.Cut(entry, "\n\t},\n")
	if !ok || !ok2 {
		t.Fatalf("no listThings entry in the table:\n%s", gen)
	}
	if !strings.Contains(entry, `200: {Ref: ""},`) {
		t.Errorf("the 200 is missing from the table entry:\n%s", entry)
	}
	for _, key := range []string{"4XX", "default"} {
		if strings.Contains(entry, key) {
			t.Errorf("the table entry has %s:\n%s", key, entry)
		}
	}
}
