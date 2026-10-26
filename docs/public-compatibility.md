# Compatibility and upgrades

What stays stable between versions, and how to migrate when something
changes.

## Deprecation policy

Anything the provider retires (a resource, data source, ephemeral resource,
list resource, action or attribute) is deprecated first, and keeps working
until the next major version.

- **It warns** on every validate and plan that uses it. The warning names
  what replaces it and the version that removes it, for example "Use
  `network_id` instead. `vpc_id` is removed in v1.0.0." The registry page
  marks it Deprecated.
- **It's announced** under NOTES in [`CHANGELOG.md`](../CHANGELOG.md).
- **It keeps working until the next major version.** Below 1.0, that's
  1.0.0.
- **Its removal is a breaking change**, listed under BREAKING CHANGES, with
  the migration on this page.

When the platform removes an operation or field, the provider can't keep it
working. That goes under BREAKING CHANGES in the next release, with a
migration here.

## State upgrades

When a new version changes how a resource is stored in state, the provider
upgrades the state the first time Terraform reads it. There's nothing to run.

**You can't go back afterwards.** An older provider can't read the upgraded
state, and Terraform stops with "Resource instance managed by newer provider
version". To keep that option, save the state before upgrading:

```sh
terraform state pull > before-upgrade.tfstate
```

To go back, pin the older provider and restore the copy. `-force` is needed
because Terraform refuses to push an older state. Anything the newer provider
recorded since is lost.

```sh
terraform state push -force before-upgrade.tfstate
```

Attributes added in a new version read as null until the first refresh.

## Import

Every resource imports by its id, or by parent and id for a subnet or a
peering:

```sh
terraform import k0rdent_kubernetes_cluster.example cluster-id
```

- On Terraform 1.12 and later, an `import` block can name the parts with
  `identity` instead.
- A `region/project/id` form imports from outside the provider's region and
  project. The resource block then has to set the same `region` and
  `project`, or the next plan replaces it.

The import id forms and the identity attributes are part of the stable
interface.

## Terraform and cluster types

- Terraform 1.10 or later. See the [support matrix](support-matrix.md).
- An unversioned `cluster_type` stays as written, and the version the
  platform picked is in `cluster_type_resolved`.
- Naming a different cluster type replaces the cluster.
- Naming a later `available` version of the same cluster type upgrades it in
  place, one version per step.
- Naming an earlier version fails the plan, since upgrades only go forward.

## Migrations

### Tenant quota renamed to organization quota

The platform's quota API now calls the organization's scope `organization`
rather than `tenant`. `k0rdent_organization_quota` replaces
`k0rdent_tenant_quota`, and reads the same on a platform either side of the
rename:

```hcl
# Before
data "k0rdent_tenant_quota" "org" {
  organization_id = "org-example"
}

# After
data "k0rdent_organization_quota" "org" {
  organization_id = "org-example"
}
```

- **Rename each reference** from `data.k0rdent_tenant_quota` to
  `data.k0rdent_organization_quota`. A data source keeps nothing in state
  that needs moving.
- **Compare scopes with `organization`**, not `tenant`, in `binding_scope`
  and `scopes[*].scope` of `k0rdent_organization_quota` and
  `k0rdent_project_quota`.
- **`k0rdent_tenant_quota` keeps working until v1.0.0**, with a warning, and
  still reports `tenant`.

### Projects locked to a region

Every project now belongs to one region, set when it's created.
`k0rdent_project` takes `region`:

```hcl
resource "k0rdent_project" "production" {
  organization_id = "org-ml-platform"
  region          = "us-texas-1" # leave out to use the provider's region
}
```

- **A new project needs a region**, on the resource or the provider. It has
  to be one listed in `project_region_ids` of the `k0rdent_regions` data
  source.
- **Changing the region replaces the project.** The provider asks for
  `allow_replace = true` first, and the platform refuses to delete a project
  that still has clusters.
- **An existing project needs no change** if its configuration leaves
  `region` out. The first refresh reads its region. A project made before
  regions existed has none, and setting `region` on it fails the plan.
- **Regional resources must be in their project's region.** The platform
  refuses anything else with `409 PROJECT_REGION_MISMATCH`, and the provider
  catches it at plan where it can.
- **`global` works as a region** where the platform hosts projects in it.

### Quota read reshaped

`limits`, `used` and `remaining` are gone from `k0rdent_tenant_quota`. Each
limit is now an entry in `dimensions`:

```hcl
# Before
min(8, data.k0rdent_tenant_quota.org.remaining["node_count"])

# After. headroom is null when nothing limits the dimension, so coalesce()
# falls back to 8.
min(8, coalesce(data.k0rdent_tenant_quota.org.dimensions["node_count"].headroom, 8))
```

Usage is in `dimensions["<name>"].scopes[*].used`, and is null when the
platform doesn't enforce that dimension.

### Kubeconfig body moved to the ephemeral resource

`download` and `raw` are gone from the `k0rdent_cluster_kubeconfig` data
source, because they saved a working credential in state. Read the body from
the ephemeral resource instead, which is never saved:

```hcl
ephemeral "k0rdent_cluster_kubeconfig" "training" {
  cluster_id = k0rdent_kubernetes_cluster.training.id
}
```

### Provider renamed from `k0rdentai`

Pre-release builds used `k0rdentai_kubernetes_cluster` and
`k0rdentai_instance_group`, installed as `retrospct/k0rdentai`.
`terraform state mv` can't move between the names. Do this instead.

First, move the state to this provider:

```sh
terraform state replace-provider registry.terraform.io/retrospct/k0rdentai registry.terraform.io/k0rdent/k0rdent
```

Then rename each resource block, and add a `moved` block for it:

```hcl
moved {
  from = k0rdentai_kubernetes_cluster.example
  to   = k0rdent_kubernetes_cluster.example
}
```

The next plan shows each resource as moved, with an in-place update for
`region` and `project`. If the state is lost, import the resource by its id
with the right region and project set.
