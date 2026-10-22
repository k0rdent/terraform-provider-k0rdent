# Waits for the cluster's kubeconfig, so it works in the same apply that
# creates the cluster.
ephemeral "k0rdent_cluster_kubeconfig" "training" {
  # Read the kubeconfig after the cluster exists. `id` is known at plan time,
  # but `state` isn't until the create has run.
  cluster_id = k0rdent_kubernetes_cluster.training.state != null ? k0rdent_kubernetes_cluster.training.id : null

  timeouts {
    open = "10m"
  }

  lifecycle {
    # Stop the run if the kubeconfig has nothing the providers below can use.
    # Otherwise they'd fall back to KUBE_* settings and maybe another cluster.
    postcondition {
      condition     = self.host != null && self.token != null
      error_message = "The kubeconfig carries nothing to hand over, so the kubernetes provider would fall back to KUBE_* settings."
    }
  }
}

# A cluster outside the provider's scope needs its scope named here too.
ephemeral "k0rdent_cluster_kubeconfig" "elsewhere" {
  cluster_id = "training-02"
  region     = "eu-west-1"
  project    = "cp-research"
}

# `cluster_ca_certificate` is already PEM, so it needs no base64decode. `raw`
# holds the whole kubeconfig YAML.
provider "kubernetes" {
  host                   = ephemeral.k0rdent_cluster_kubeconfig.training.host
  cluster_ca_certificate = ephemeral.k0rdent_cluster_kubeconfig.training.cluster_ca_certificate
  token                  = ephemeral.k0rdent_cluster_kubeconfig.training.token
}

# helm 3.x. On 2.x, `kubernetes` is a block, not an attribute.
provider "helm" {
  kubernetes = {
    host                   = ephemeral.k0rdent_cluster_kubeconfig.training.host
    cluster_ca_certificate = ephemeral.k0rdent_cluster_kubeconfig.training.cluster_ca_certificate
    token                  = ephemeral.k0rdent_cluster_kubeconfig.training.token
  }
}
