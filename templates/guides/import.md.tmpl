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
- Under OpenTofu, write only literal values in `identity`, or `each.key`
  with `for_each`. OpenTofu 1.12 and 1.13 don't wait for anything else
  `identity` refers to: a data source's attribute fails the plan with
  `Invalid import identity argument`, and a variable can be read before
  it's set. For a value from a data source or a variable, use `id` in place
  of `identity`.

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
replaces the resource. The plan warns you when this applies.

Each resource's page lists its import forms.

## After the import, run a plan

If your configuration matches what the platform has, the plan is empty.

Some values only the configuration knows, so the plan after an import shows an
in-place update for them. None of these is a replacement, and the apply
changes nothing on the platform:

- **A cluster's `node_pool`**, when you name only some pools. The import
  adopts every pool. Naming every pool, or leaving `node_pool` out, plans
  clean.
- **An InfiniBand network's requested `pkey`.** The import records the
  allocated value.
- **A subnet's `allocate_gateway`** on a pool that isn't routable.
- **A `cluster_type` pinned to the newest available version.** The import
  records the unversioned reference (the cluster type's `self_link`) when the
  cluster is on the newest version, and the pinned version otherwise.
  `cluster_type_resolved` always holds the bound version.
- **A tag set both in `tags` and in the provider's `default_tags`**, to the
  same value. The import gives it to the defaults.
- **`allow_replace` and `timeouts`**, which never reach the platform.
- **SSH keys written in a user's `ssh_public_keys`**, on a cluster or an
  instance group. The platform answers references to the keys, not the keys,
  so the import records the configured keys once it has checked them against
  the platform (see below). Write the keys in `ssh_public_keys_wo` instead and
  the plan is empty.

`ssh_public_keys_wo` is write-only, so it needs Terraform 1.11 or OpenTofu
1.11 or later.

The provider checks the configured keys, in either attribute, against the
keys on the nodes. After a create it has the keys it sent. After an import it
reads each key's fingerprint from the platform. A different key plans a
replacement, since the platform sets keys only when nodes are made.

Reading a fingerprint needs `credentials.metadata.read` bound in the
resource's project. Resources the account creates need nothing, since the
provider checks them against the keys it sent. A binding across the
organization doesn't reach its projects, because the platform passes only
compute roles down. So an organization-scoped account, such as the one the
walkthrough uses, needs the role bound in each project it imports into.

The narrowest role holds only that permission, and can't read a credential's
secret or change one. An organization admin creates it once, with
`POST /v1/regions/global/organizations/<org>/iam/roles`:

```json
{
  "id": "ssh-key-reader",
  "displayName": "SSH key reader",
  "permissions": ["credentials.metadata.read"],
  "scopes": ["project"]
}
```

Then binds it in each project, with
`POST /v1/regions/global/organizations/<org>/iam/policies`:

```json
{
  "role": "ssh-key-reader",
  "principal": { "type": "serviceAccount", "id": "<service account id>" },
  "scope": { "type": "project", "resource": "<project>" }
}
```

The built-in `credentials-admin` also has the permission, but it can change
and revoke credentials too.

Without it:

- The import warns that it can't check the keys.
- A plan that sets keys warns and shows an update. The apply stops with an
  error and changes nothing.
- Leaving the keys out plans clean. `ssh_access` keeps the platform's users
  and keys.
- A destroy runs as usual.

A key whose credential is revoked in the console warns on every plan. Revoking
can't take a key off nodes that are running, so they still accept it. To
remove it, change the key, which replaces the resource.

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

## Recover a cluster after a lost runner

If a runner dies after the platform accepts a create and before Terraform
saves state, a normal apply with the same `create_token` adopts the cluster on
its next readback. A delete flow can adopt before destroy without creating an
absent cluster, with no query commands, extra workspaces, import commands or
state edits. Put this module around the managed resource:

```hcl
terraform {
  required_providers {
    k0rdent = { source = "k0rdent/k0rdent" }
  }
}

variable "adopt_only" {
  type    = bool
  default = false
}

variable "cluster_id" {
  type = string
}

variable "region" {
  type    = string
  default = null
}

variable "project" {
  type    = string
  default = null
}

variable "create_token" {
  type      = string
  ephemeral = true
}

data "k0rdent_kubernetes_clusters" "recovery" {
  count   = var.adopt_only ? 1 : 0
  id      = var.cluster_id
  region  = var.region
  project = var.project
}

locals {
  adoptees = var.adopt_only ? {
    for cluster in data.k0rdent_kubernetes_clusters.recovery[0].clusters : cluster.id => cluster
    } : {
    (var.cluster_id) = {
      id      = var.cluster_id
      region  = var.region
      project = var.project
    }
  }
}

import {
  for_each = var.adopt_only ? local.adoptees : {}
  to       = k0rdent_kubernetes_cluster.test[each.key]
  identity = {
    id      = each.key
    region  = each.value.region
    project = each.value.project
  }
}

resource "k0rdent_kubernetes_cluster" "test" {
  for_each          = local.adoptees
  id                = each.key
  region            = each.value.region
  project           = each.value.project
  cluster_type      = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"
  adopt_only        = var.adopt_only
  create_token      = var.create_token
  display_name      = "acceptance"
  node_pool = {
    "gpu-workers" = { node_count = 2 }
  }
  timeouts {
    create = "10m"
  }
}
```

Set `adopt_only = true`, keep the same cluster id, token, region and project,
then apply once before the normal destroy:

```bash
terraform init
terraform plan -out=adopt.tfplan
terraform apply adopt.tfplan
terraform plan -destroy -out=destroy.tfplan
terraform apply destroy.tfplan
```

The lookup returns at most one cluster. No match means both `for_each` maps
are empty, so the apply creates nothing and records no managed cluster. An
exact match imports that cluster with its UID and sends no create request. A
same-named cluster with a missing or different token fails the plan after its
import read but before a state write. UID guards the subsequent destroy. A cluster still
creating is allowed to settle first, after which the same destroy applies.

A run interrupted while the create waits, such as by a CI cancel or a job
timeout, saves the cluster untainted when `create_token` is set. Terraform
still ends that run with `execution halted`. The next apply, normal or
adopt-only, waits for the same cluster. A row Terraform holds tainted, such as
after a create the platform failed, is replaced instead: the adopt-only apply
deletes the cluster and then fails with no cluster to adopt. Its plan warns
about this before the apply.

If provenance changes between a saved plan and its apply, the import completes
before the apply refuses it. No create or delete is sent. The failed apply
leaves the imported row for the old UID in state. The next plain plan leaves that row in place and
refuses the changed provenance. `terraform destroy` refuses it too and sends no
delete or cancel.

The token is checked only by the managed resource, where it's write-only and
never enters plan or state. Keep it outside those files and use the same
value when applying the saved plan. Setting `adopt_only` back to `false`
keeps the same resource address, so it doesn't replace the adopted cluster
and ordinary creates still work.

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
