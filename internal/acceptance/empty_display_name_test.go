package acceptance

import (
	"runtime"
	"strings"
	"testing"
)

// `display_name = ""` is a value a configuration can write. An update sends it
// as it is, and the answer may leave the field out, which must not read back
// as null: Terraform fails the apply as an inconsistent result.
func TestAccAnEmptyDisplayNameSurvivesAnUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	for _, tc := range []struct{ typeName, name string }{
		{clusterType, "acc-empty-name-cluster"},
		{instanceGroupType, "acc-empty-name-group"},
	} {
		t.Run(tc.typeName, func(t *testing.T) {
			c := newCLIRun(t)
			cfg := heldObject(tc.typeName, tc.name, "")
			c.write(cfg)
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("apply:\n%s", out)
			}
			t.Cleanup(func() {
				if out, ok := c.run("destroy", "-auto-approve"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})
			c.write(strings.Replace(cfg, `display_name = ""`, "display_name = \"\"\n  description  = \"changed\"", 1))
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the update with an empty display_name failed:\n%s", out)
			}
			if out, ok := c.run("plan", "-detailed-exitcode"); !ok {
				t.Errorf("the replan isn't empty:\n%s", out)
			}
		})
	}
}
