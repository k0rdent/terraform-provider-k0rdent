---
page_title: "Getting started"
subcategory: ""
description: |-
  From a credential to a running tenant cluster in one configuration.
---

# Getting started

One configuration, from nothing to a cluster you can reach.

You need:

- Terraform 1.10 or later, for ephemeral resources.
- A service account's client id and secret. The authentication guide says how
  to get them.

## 1. Configure the provider

```hcl
terraform {
  required_providers {
    k0rdent = {
      source  = "k0rdent/k0rdent"
      version = "~> 0.1"
    }
  }
}

provider "k0rdent" {
  endpoint = "https://api.k0rdent.ai"
  region   = "us-texas-1"
  project  = "cp-production"
}
```

Each value can come from `K0RDENT_ENDPOINT`, `K0RDENT_REGION` or
`K0RDENT_PROJECT` instead. If you don't know your region or project, list
them. This is also a quick check that your credential works:

```hcl
data "k0rdent_regions" "all" {}
data "k0rdent_projects" "mine" {}

output "regions" { value = data.k0rdent_regions.all.ids }
output "projects" { value = data.k0rdent_projects.mine.ids }
```

## 2. Pick a cluster type

A **ClusterType** sets the machine types, image and Kubernetes version. Your
operator publishes them, and you create a cluster from one.

```hcl
data "k0rdent_cluster_types" "available" {}

# The list doesn't include node pools, so read the one you picked.
data "k0rdent_cluster_type" "training" {
  id = "gpu-training"
}

# Lists the cluster type's node pools, so you know each pool's id.
output "node_pools" {
  value = data.k0rdent_cluster_type.training.node_pools
}
```

Use a worker pool's `id` in the next step. Control-plane pools can't be resized.

## 3. Create the cluster

```hcl
resource "k0rdent_kubernetes_cluster" "training" {
  id           = "ml-training"
  cluster_type = data.k0rdent_cluster_type.training.self_link
  display_name = "ML training"

  tags = {
    team = "ml-platform"
  }

  # Size the worker pool by its id from step 2. Pools you leave out keep
  # their defaults.
  node_pool = {
    gpu-workers = {
      node_count = 8
    }
  }

  # Building hardware can take more than 2 hours, so creates default to 3h.
  timeouts {
    create = "3h"
  }
}
```

`apply` returns once the cluster is ready, not when the request is accepted,
so you can build on it in the same run.

## 4. Connect to it

The kubeconfig is an **ephemeral resource**, so it's never written to state.

```hcl
ephemeral "k0rdent_cluster_kubeconfig" "training" {
  # Read the kubeconfig after the cluster exists. `id` is known at plan time,
  # but `state` isn't until the create has run.
  cluster_id = k0rdent_kubernetes_cluster.training.state != null ? k0rdent_kubernetes_cluster.training.id : null

  lifecycle {
    # Stop the run if the kubeconfig has nothing the providers below can use.
    # Otherwise they'd fall back to KUBE_* settings and maybe another cluster.
    postcondition {
      condition     = self.host != null && self.token != null
      error_message = "The kubeconfig carries nothing to hand over, so the kubernetes provider would fall back to KUBE_* settings."
    }
  }
}

provider "kubernetes" {
  host                   = ephemeral.k0rdent_cluster_kubeconfig.training.host
  cluster_ca_certificate = ephemeral.k0rdent_cluster_kubeconfig.training.cluster_ca_certificate
  token                  = ephemeral.k0rdent_cluster_kubeconfig.training.token
}

provider "helm" {
  kubernetes = {
    host                   = ephemeral.k0rdent_cluster_kubeconfig.training.host
    cluster_ca_certificate = ephemeral.k0rdent_cluster_kubeconfig.training.cluster_ca_certificate
    token                  = ephemeral.k0rdent_cluster_kubeconfig.training.token
  }
}
```

- `cluster_ca_certificate` is already PEM, so it needs no `base64decode`.
- The token is short-lived and can't be renewed. If in-cluster work can run
  past it, set `relay = true` on the ephemeral resource, and the connection
  keeps working until the run ends.
- Terraform won't let an ephemeral value be saved, for example in an output or
  a file. Pass it to a provider block, as above.

## 5. Scale it

Change the count and apply.

```hcl
  node_pool = {
    gpu-workers = {
      node_count = 16
    }
  }
```

A plan right after an apply shows no changes. If it does, please report it as
a bug.

## Next

- **Authentication**: service accounts, rotation, and what a credential can
  reach.
- **Import**: adopt clusters created in the console.
- **Known limitations**: where a plan can't tell you what the apply will do.
  Read it before your first large configuration.
