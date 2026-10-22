---
page_title: "Importing an existing estate"
subcategory: ""
description: |-
  Adopting resources built through the console or the API, and what cannot be
  imported.
---

# Importing an existing estate

Every managed resource can be imported, so you can adopt anything built in the
console or through the API without rebuilding it. The provider doesn't claim
ownership, so you can keep using the API alongside it.

## Three ways to import

On Terraform 1.12 and later, an `import` block can name the resource by
identity:

```hcl
import {
  to = k0rdent_kubernetes_cluster.training
  identity = {
    region  = "us-texas-1"
    project = "cp-production"
    id      = "ml-training"
  }
}
```

- `region` and `project` are optional and default to the provider's.
- Some resources need more. A subnet adds `network`, a peering adds `vpc`, a
  security group binding takes only `vpc`, and a project takes
  `organization_id` and `id`. Each resource's page lists its identity.

On Terraform 1.5 and later, an `import` block can take a string id:

```hcl
import {
  to = k0rdent_kubernetes_cluster.training
  id = "us-texas-1/cp-production/ml-training"
}
```

On any version, use the command:

```bash
terraform import k0rdent_kubernetes_cluster.training us-texas-1/cp-production/ml-training
```

Prefer the `import` block: the adoption shows up in the plan for review. All
three give the same state.

## Import ids

The short form uses the provider's region and project:

```bash
terraform import k0rdent_kubernetes_cluster.training ml-training
```

The long form names them:

```bash
terraform import k0rdent_kubernetes_cluster.training us-texas-1/cp-production/ml-training
```

If you use the long form, or an identity with `region` or `project`, set the
same `region` and `project` in the resource block. Otherwise the next plan
replaces the resource. The import warns you when this applies.

Each resource's page lists its import forms.

## After the import, run a plan

If your configuration matches what the platform has, the plan is empty.

Some values only the configuration knows, so the plan after an import shows an
in-place update for them. None of these is a replacement, and the apply
changes nothing on the platform:

- **A cluster's `node_pool`**, when you name only some pools. The import
  adopts every pool. Naming every pool, or leaving `node_pool` out, plans
  clean.
- **A network's `vlan_id = "auto"`**, or an InfiniBand network's requested
  `pkey`. The import records the allocated value.
- **A subnet's `allocate_gateway`** on a pool that isn't routable.
- **A `cluster_type` pinned to the newest available version.** The import
  records the unversioned reference (the cluster type's `self_link`) when the
  cluster is on the newest version, and the pinned version otherwise.
  `cluster_type_resolved` always holds the bound version.
- **A tag set both in `tags` and in the provider's `default_tags`**, to the
  same value. The import gives it to the defaults.
- **`allow_replace` and `timeouts`**, which never reach the platform.

A security group binding that holds only its VPC's default group imports as
`security_groups = []`. Write it that way to plan clean. Naming the default
group plans an update that sends the same group again.

One import does change the platform. A webhook subscription's signing secret
can't be read back. If your configuration sets `secret_wo` and
`secret_wo_version`, the first apply rotates the subscription to that secret.
It keeps its id and queued events, and with the default `rotation_overlap` the
old secret keeps working for 24 hours. Leave the secret out to plan clean.

## Generate the configuration

```bash
terraform plan -generate-config-out=generated.tf
```

This writes a configuration from what the import finds, including every node
pool and the allocated VLAN id. Edit it down rather than writing from scratch.

## What can't be imported

- **Data sources.** They only read, and have no state.
- **The `k0rdent_cluster_kubeconfig` ephemeral resource.** Ephemeral
  resources are never saved to state.
- **Anything the provider doesn't manage**, such as VPCs, node pools on their
  own, cluster types, machine types, address pools, servers and reservations.
  See the known limitations page.

## Adopt many at once

On Terraform 1.14 and later, `terraform query` finds what exists and writes
the configuration to adopt it. Each managed resource has a list resource of
the same name. Put `list` blocks in a file ending `.tfquery.hcl`, next to the
configuration with the provider block:

```hcl
# adopt.tfquery.hcl
list "k0rdent_kubernetes_cluster" "found" {
  provider = k0rdent
}

list "k0rdent_file_store" "found" {
  provider = k0rdent
}

list "k0rdent_security_group" "found" {
  provider = k0rdent
}
```

Then write an `import` and a `resource` block for each one found, and plan:

```bash
terraform query -generate-config-out=adopted.tf
terraform plan
```

The plan shows each object as an import with no changes. Review `adopted.tf`
for the values listed under "After the import, run a plan".

Options on each `list` block:

- `config` narrows the search. A project-scoped list uses the provider's
  project unless `config` names another.
- `limit` defaults to 100. A list cut short says so.
- `include_resource = true` shows each object's full attributes.
- Networks, subnets and security groups the platform made itself, such as a
  VPC's default group, are skipped unless `include_platform_owned = true`.
- Objects being deleted, or that can't be read, are skipped. The ones that
  can't be read are named in a warning.

Each list resource's page lists its filters.

On older Terraform, write one `import` block per resource. `for_each` adopts
several at once:

```hcl
locals {
  clusters = ["ml-training", "ml-inference"]
}

import {
  # One import per cluster id. Each id becomes the instance key and the
  # identity's id, and region and project come from the provider.
  for_each = toset(local.clusters)
  to       = k0rdent_kubernetes_cluster.adopted[each.key]
  identity = {
    id = each.key
  }
}
```
