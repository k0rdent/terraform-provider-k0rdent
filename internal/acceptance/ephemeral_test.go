package acceptance

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// An ephemeral resource opens during plan, so reading the kubeconfig of a
// cluster created in the same run only works when `cluster_id` is unknown at
// plan time. The cluster's `id` is configured, so the config gates it on
// `state`, which is unknown until the create runs. `depends_on` would be the
// idiom, and the data source needs nothing at all, but Terraform 1.16.3 opens
// an ephemeral resource at plan when its arguments are known: see
// TestAccEphemeralKubeconfigDependsOnDoesNotDeferTheOpen. That's the shape
// the flagship journey uses: create a cluster, hand its kubeconfig to a
// Kubernetes provider, all in one apply.
//
// The fake answers 404 for the first reads after the cluster is active, as the
// platform does while it issues the kubeconfig, so this also proves the wait.
//
// The postcondition is what checks the value. It runs inside Terraform when the
// ephemeral opens, so the kubeconfig is never written to state, a file or the
// trace log that gate S4 searches. `raw` is sensitive, so a failing condition
// prints that it is, not what it holds. Asserting on the cluster's id rather
// than on the shape alone catches a kubeconfig for the wrong cluster too.
//
// The second postcondition is the hand-off a kubernetes provider takes: the
// fake's server for this cluster and its token, and no CA, because the fake's
// kubeconfig carries none.
func TestAccEphemeralKubeconfigInTheSameRun(t *testing.T) {
	kubeconfigType := provider.ResourceName("cluster_kubeconfig")
	cfg := fmt.Sprintf(`
resource %q "eph" {
  id           = "acc-eph"
  cluster_type = %q
  node_pool = { "gpu-workers" = { node_count = 1 } }
}

ephemeral %q "eph" {
  # depends_on doesn't defer an ephemeral open (Terraform 1.16.3), so gate on state.
  cluster_id = %[4]s.eph.state != null ? %[4]s.eph.id : null

  lifecycle {
    postcondition {
      condition = (
        try(yamldecode(self.raw).kind, "") == "Config" &&
        try(yamldecode(self.raw)["current-context"], "") == self.cluster_id
      )
      error_message = "The opened kubeconfig is not a Config whose current context is the new cluster."
    }
    postcondition {
      condition = (
        self.host == "https://${self.cluster_id}.fake.invalid:6443" &&
        self.token != null &&
        self.cluster_ca_certificate == null &&
        self.client_certificate == null
      )
      error_message = "The parsed connection is not the new cluster's server and token."
    }
  }
}
`, clusterType, clusterTypeRef, kubeconfigType, clusterType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.TestCheckResourceAttrSet(
				clusterType+".eph", "id"),
		}},
	})
}

// The metadata read waits out the same window: in one apply it runs straight
// after the cluster reports active, before the kubeconfig is issued.
//
// It takes plain `.id`, with no `depends_on`: the reference alone makes the
// data source depend on the cluster, and Terraform defers a data source's
// read while a resource it depends on has changes to apply.
func TestAccKubeconfigMetadataForANewCluster(t *testing.T) {
	kubeconfigType := provider.ResourceName("cluster_kubeconfig")
	cfg := fmt.Sprintf(`
resource %q "meta" {
  id           = "acc-eph-meta"
  cluster_type = %q
  node_pool = { "gpu-workers" = { node_count = 1 } }
}

data %q "meta" {
  cluster_id = %[4]s.meta.id
}
`, clusterType, clusterTypeRef, kubeconfigType, clusterType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.TestCheckResourceAttr(
				"data."+kubeconfigType+".meta", "status", "active"),
		}},
	})
}

// A 404 because the cluster does not exist is not the transient one, and must
// not cost the whole timeout.
func TestAccEphemeralKubeconfigForAMissingClusterFailsAtOnce(t *testing.T) {
	kubeconfigType := provider.ResourceName("cluster_kubeconfig")
	cfg := fmt.Sprintf(`
ephemeral %q "missing" {
  cluster_id = "no-such-cluster"
  timeouts {
    open = "10m"
  }
}
`, kubeconfigType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      cfg,
			ExpectError: regexp.MustCompile(`(?s)cluster "no-such-cluster" does not exist`),
		}},
	})
}

// The platform issues a kubeconfig once, when the cluster settles, and drops a
// failed issue. The fake's marker makes that issue fail. The open has to give
// up after the provisional window rather than wait out `timeouts.open`.
func TestAccEphemeralKubeconfigThatIsNeverIssuedGivesUp(t *testing.T) {
	kubeconfigType := provider.ResourceName("cluster_kubeconfig")
	cfg := fmt.Sprintf(`
resource %q "unissued" {
  id           = "acc-eph-unissued"
  cluster_type = %q
  display_name = "fake-kubeconfig-fails"
  node_pool = { "gpu-workers" = { node_count = 1 } }
}

ephemeral %q "unissued" {
  # depends_on doesn't defer an ephemeral open (Terraform 1.16.3), so gate on state.
  cluster_id = %[4]s.unissued.state != null ? %[4]s.unissued.id : null
  timeouts {
    open = "20m"
  }
}
`, clusterType, clusterTypeRef, kubeconfigType, clusterType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{{
			Config:      cfg,
			ExpectError: regexp.MustCompile(`(?s)has not issued it a\s+kubeconfig.*the\s+fake\s+refused\s+to\s+issue`),
		}},
	})
}

// Why the ephemeral examples gate `cluster_id` on `state` rather than use
// `depends_on`: Terraform opens an ephemeral resource at plan when its
// arguments are known, depends_on or not, so the open meets a cluster that
// doesn't exist yet. Recorded on Terraform 1.16.3. When a Terraform release
// defers it, this fails, and the examples can move to depends_on.
func TestAccEphemeralKubeconfigDependsOnDoesNotDeferTheOpen(t *testing.T) {
	kubeconfigType := provider.ResourceName("cluster_kubeconfig")
	cfg := fmt.Sprintf(`
resource %q "deps" {
  id           = "acc-eph-deps"
  cluster_type = %q
  node_pool = { "gpu-workers" = { node_count = 1 } }
}

ephemeral %q "deps" {
  cluster_id = %[4]s.deps.id
  depends_on = [%[4]s.deps]
}
`, clusterType, clusterTypeRef, kubeconfigType, clusterType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{{
			Config:      cfg,
			ExpectError: regexp.MustCompile(`(?s)pre-apply plan.*cluster "acc-eph-deps" does not exist`),
		}},
	})
}
