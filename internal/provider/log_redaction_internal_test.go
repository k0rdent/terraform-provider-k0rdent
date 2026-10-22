package provider

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// redactDiagnosticText is the boundary before TF_LOG as well as before a
// diagnostic, and an error's text can carry whatever the platform or the
// token endpoint answered. So every log field holding one goes through it.
func TestEveryLoggedErrorIsRedacted(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	logged := regexp.MustCompile(`"error"\]?(?::|\s*=)\s*([^,}\n]+)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range logged.FindAllSubmatch(src, -1) {
			if v := string(m[1]); strings.Contains(v, ".Error()") && !strings.HasPrefix(v, "redactDiagnosticText(") {
				t.Errorf("%s logs %s unredacted", f, v)
			}
		}
	}
}
