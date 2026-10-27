package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The header name is an argument because Terraform's trace log holds the
// command, and gate S4 refuses a log with an Authorization header in it.
const relayCall = `
sleep "$PAUSE"
ca=$(mktemp)
printf '%s' "$CA" > "$ca"
printf '%s: Bearer %s\n' Authorization "$TOKEN" | curl -sS --fail-with-body --cacert "$ca" -H @- \
  -X POST --data '{"kind":"ConfigMap"}' -o "$OUT" "$HOST/api/v1/namespaces/default/configmaps"
rc=$?
rm -f "$ca"
exit $rc
`

func relayConfig(id, displayName string, relay bool, outDir string) string {
	kubeconfigType := provider.ResourceName("cluster_kubeconfig")
	call := func(name, pause string) string {
		return fmt.Sprintf(`
  provisioner "local-exec" {
    command = <<-EOT
%s
EOT
    environment = {
      HOST  = ephemeral.%s.c.host
      CA    = ephemeral.%s.c.cluster_ca_certificate
      TOKEN = ephemeral.%s.c.token
      PAUSE = %q
      OUT   = %q
    }
  }`, relayCall, kubeconfigType, kubeconfigType, kubeconfigType, pause, filepath.Join(outDir, name+".json"))
	}
	return fmt.Sprintf(`
resource %[1]q "c" {
  id           = %[2]q
  cluster_type = %[3]q
  display_name = %[4]q
  node_pool = { "gpu-workers" = { node_count = 1 } }
}

ephemeral %[5]q "c" {
  cluster_id = %[1]s.c.state != null ? %[1]s.c.id : null
  relay      = %[6]t
}

resource "terraform_data" "first" {
%[7]s
}

resource "terraform_data" "later" {
  depends_on = [terraform_data.first]
%[8]s
}
`, clusterType, id, clusterTypeRef, displayName, kubeconfigType, relay, call("first", "0"), call("later", "7"))
}

func clusterAPILines(t *testing.T, id string) []string {
	t.Helper()
	body, err := os.ReadFile(fakeLogPath)
	if err != nil {
		t.Fatalf("reading the fake's log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, "cluster-api ") && strings.Contains(line, "/clusters/"+id+"/") {
			lines = append(lines, line[strings.Index(line, "cluster-api "):])
		}
	}
	return lines
}

func readOut(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name+".json")) //nolint:gosec // a file this test's run wrote
	if err != nil {
		t.Fatalf("reading what the %s call got: %v", name, err)
	}
	return string(b)
}

func checkClusterAPI(t *testing.T, id string, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got := clusterAPILines(t, id)
		var statuses []string
		for _, line := range got {
			f := strings.Fields(line)
			statuses = append(statuses, strings.Join(f[3:], " "))
		}
		if strings.Join(statuses, ", ") != strings.Join(want, ", ") {
			return fmt.Errorf("the cluster answered %q, want %q", statuses, want)
		}
		return nil
	}
}

func TestAccKubeconfigRelayOffFailsPastTheToken(t *testing.T) {
	out := t.TempDir()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{{
			Config:      relayConfig("acc-relay-off", "fake-kubeconfig-expires", false, out),
			ExpectError: regexp.MustCompile(`(?s)terraform_data\.later.*exit status 22`),
		}},
	})
	if got := readOut(t, out, "first"); !strings.Contains(got, `"status":"Success"`) {
		t.Errorf("the first call got %s", got)
	}
	if got := readOut(t, out, "later"); !strings.Contains(got, `"code":401`) {
		t.Errorf("the later call got %s, want the cluster's 401", got)
	}
	if err := checkClusterAPI(t, "acc-relay-off", "200", "401 expired")(nil); err != nil {
		t.Error(err)
	}
}

func TestAccKubeconfigRelayRenewsBeforeTheTokenExpires(t *testing.T) {
	out := t.TempDir()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{{
			Config: relayConfig("acc-relay-jwt", "fake-kubeconfig-expires", true, out),
			// The relay renews before `exp`, so no 401.
			Check: checkClusterAPI(t, "acc-relay-jwt", "200", "200"),
		}},
	})
	if got := readOut(t, out, "later"); !strings.Contains(got, `"status":"Success"`) {
		t.Errorf("the later call got %s", got)
	}
}

func TestAccKubeconfigRelayReplaysAfterAnOpaqueTokenExpires(t *testing.T) {
	out := t.TempDir()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{{
			Config: relayConfig("acc-relay-opaque", "fake-kubeconfig-expires-opaque", true, out),
			// The fake answers 400 to an empty POST, so the last 200 means the body was replayed.
			Check: checkClusterAPI(t, "acc-relay-opaque", "200", "401 expired", "200"),
		}},
	})
}

func TestAccKubeconfigRelayReportsAPlatformThatWontReissue(t *testing.T) {
	out := t.TempDir()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{{
			Config:      relayConfig("acc-relay-refused", "fake-kubeconfig-expires fake-kubeconfig-reissue-denied", true, out),
			ExpectError: regexp.MustCompile(`(?s)terraform_data\.later.*exit status 22`),
		}},
	})
	got := readOut(t, out, "later")
	for _, want := range []string{`"code":502`, "could not fetch a new kubeconfig", "may no longer read the kubeconfig"} {
		if !strings.Contains(got, want) {
			t.Errorf("the later call got %s, want it to say %q", got, want)
		}
	}
	// The expired token never reached the cluster.
	if err := checkClusterAPI(t, "acc-relay-refused", "200")(nil); err != nil {
		t.Error(err)
	}
}
