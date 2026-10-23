package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Text the platform writes, a display name, a tag key, a kubeconfig's CA
// path, reaches the terminal through these builders, so an escape sequence in
// it has to arrive escaped.
func TestPlatformTextReachesWarningsEscaped(t *testing.T) {
	const esc = "\x1b]0;pwn\x07"
	tags, _ := types.MapValueFrom(context.Background(), types.StringType, map[string]string{"k" + esc: "v"})
	kubeconfig := "apiVersion: v1\nkind: Config\ncurrent-context: c\ncontexts:\n- name: c\n  context: {cluster: x, user: u}\n" +
		"clusters:\n- name: x\n  cluster: {server: \"https://h\", certificate-authority: \"/x\\e]0;pwn\\a\"}\n" +
		"users:\n- name: u\n  user: {token: t}\n"
	_, warnings := parseKubeconfigConnection(kubeconfig)
	for name, text := range map[string]string{
		"list display name":          displayName(ptr("a"+esc+"b"), "c1"),
		"describeTags key":           describeTags(tags),
		"kubeconfig CA file warning": strings.Join(warnings, "\n"),
	} {
		if text == "" || strings.ContainsAny(text, "\x1b\x07") {
			t.Errorf("%s: %q", name, text)
		}
	}
}

// The region list is platform text too, and the no-region error names its
// ids.
func TestTheNoRegionErrorEscapesTheRegionList(t *testing.T) {
	api := &regionsAPI{regions: []map[string]any{region("r1\x1b]0;pwn\x07", true)}}
	resp := planProject(t, regionsMeta(t, api, ""), nil, nil)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a project with no region planned")
	}
	if d := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(d, "r1") || strings.ContainsAny(d, "\x1b\x07") {
		t.Errorf("detail = %q", d)
	}
}
