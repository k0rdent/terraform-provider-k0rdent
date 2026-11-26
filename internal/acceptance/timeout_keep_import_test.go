package acceptance

import (
	"encoding/json"
	"os"
	"testing"
)

func TestAccKubernetesClusterCreateTimeoutKeepImportedCreate(t *testing.T) {
	t.Parallel()
	if os.Getenv("TF_ACC") == "" {
		skipTest(t, "TF_ACC is not set")
	}
	const name = "acc-review-keep-import"
	addr := clusterType + ".test"
	creatingOutside(t, name, createsSlowly, 0)
	c := newCLIRun(t)
	c.write(importedFailed(name, createsSlowly, "  on_create_timeout = \"keep\"\n  timeouts {\n    create = \"1s\"\n  }\n"))
	t.Cleanup(func() {
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	if out, ok := c.run("import", addr, name); !ok {
		t.Fatalf("import failed:\n%s", out)
	}
	out, ok := c.run("apply", "-auto-approve", "-refresh=false")
	if ok {
		t.Fatalf("the imported create returned success at its resumed timeout:\n%s", out)
	}
	got := c.cliInstance(addr)
	if got.Status == "tainted" || got.Attributes.State != "creating" || got.Attributes.OnCreateTimeout != "keep" {
		t.Fatalf("state = %+v, want untainted creating with keep", got)
	}
	if out, planned := c.run("plan", "-out=retry.tfplan", "-refresh=false"); !planned {
		t.Fatalf("the same-configuration resume plan failed:\n%s", out)
	}
	shown, ok := c.run("show", "-json", "retry.tfplan")
	if !ok {
		t.Fatalf("showing the resume plan failed:\n%s", shown)
	}
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Before       map[string]any `json:"before"`
				After        map[string]any `json:"after"`
				AfterUnknown map[string]any `json:"after_unknown"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal([]byte(shown), &plan); err != nil || len(plan.ResourceChanges) != 1 {
		t.Fatalf("reading the resume plan: %v\n%s", err, shown)
	}
	change := plan.ResourceChanges[0]
	if change.Address != addr || change.Change.Before["on_create_timeout"] != "keep" ||
		change.Change.After["on_create_timeout"] != "keep" || change.Change.AfterUnknown["on_create_timeout"] == true {
		t.Fatalf("the resume plan did not keep the imported timeout policy:\n%s", shown)
	}
	c.write(importedFailed(name, createsSlowly, "  on_create_timeout = \"keep\"\n  timeouts {\n    create = \"2m\"\n  }\n"))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the imported create did not finish with a longer timeout:\n%s", out)
	}
	if out, code := c.runCode("plan", "-detailed-exitcode"); code != 0 {
		t.Fatalf("the plan after the imported resume exited %d:\n%s", code, out)
	}
}
