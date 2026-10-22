# Known limitations

Read this before your first large configuration. It covers:

- where a plan can't tell you exactly what the apply will do
- how the provider handles rate limits and errors
- what you still have to hardcode, and what can't be managed at all

## Where the plan can't predict the outcome

There are seven.

### 1. A cluster type reference may be resolved to a different string

`cluster_type` accepts an unversioned reference, and the platform pins it to
a version. The pinned one is in `cluster_type_resolved`, so there's no
perpetual diff, but the plan can't tell you which version you'll get. Pin the
version when it matters.

The reference can be a full URL or a path. Both are accepted.

### 2. A network's automatic VLAN id is chosen during the apply

With `evpn.vlan_id = "auto"`, the platform picks the VLAN. The plan shows
`"auto"`, and the chosen id is in `vlan_id_resolved` afterwards.

### 3. An id you leave out is generated during the apply

On a resource where `id` is optional, leaving it out makes the provider
generate one during the apply: `tf-` and 16 hex digits. The plan shows it as
known after apply.

A cluster and an instance group always need an `id`.

### 4. The platform fills in a cluster's other node pools

`node_pool` on a cluster only lists the pools you want to change. The
platform creates every pool the cluster type defines, with its default count
for any you don't name. The plan doesn't show those. They appear in state
after the apply, and aren't checked for drift.

To see the pool list before applying, read the cluster type with the
`k0rdent_cluster_type` data source.

### 5. Changing an instance group's pools can replace the group

The API can only grow an instance group's pool. Growing one updates in place.
Shrinking one, or adding or removing a pool, replaces the whole group. The
plan says `# forced replacement`, but not why.

### 6. A file store's capacity isn't a hard limit

`capacity_gib` can only grow, and the plan refuses a shrink. But storage
can use more than the capacity, and the API doesn't report how much a store
holds.

### 7. Default timeouts are estimates

Every resource that waits has a `timeouts` block with a default. Hardware
build and teardown times vary, so set the block yourself where it matters.

## Behaviour under load

**Rate limits.** The platform answers 429 when you send too many requests.
The provider retries up to six times, on every request, including the token
request.

- With a `Retry-After` header, it waits at least that long, plus up to a
  second. If that would go past the operation's deadline, or past five
  minutes when there's none, it fails at once and says how long the platform
  asked it to wait.
- Without one, it waits a random time, up to 1 second at first and doubling
  each attempt up to 30 seconds. After the last attempt it reports the
  platform's reason.

A 503 is retried the same way on a GET, PUT or DELETE, and on the few creates
and updates where the platform says a 503 changed nothing. The one you'll
meet is a VPC peering create while its VPC is busy.

A 502 or 504, or a connection that drops before an answer, is retried on a
GET, PUT or DELETE only. A refused connection is reported at once: nothing is
listening at `endpoint`.

A resent DELETE can get a 409 if the first one already worked. So a destroy
that gets a 409 reads the resource first. If it's `deleting` or gone, the
destroy waits for it. Otherwise the 409 is reported. The security group and
the file store answer a repeated delete with 204.

Nothing else is retried. A 401, a 409 during a change, a 500, and any other
5xx on a create are reported at once.

**Plan and refresh time.** See [performance](performance.md). The supported
limit is 1,000 resources in one configuration.

## Values you still have to hardcode

Some values can't be read with a tenant credential, so a configuration has to
write them as literals. The
[hardcoded values guide](guides/hardcoded-values.md) lists each one and what
has to change to remove it. In short:

- the hardware, GPU model and image behind a cluster type's machine types
- the fabric partitions a network names
- the organization for a first project, when using an API key
- zones, which no attribute takes

## What can't be managed at all

- **Network access to the Kubernetes API endpoint.** The API has no allowlist
  or source ranges.
- **VPCs.** The platform creates them from the cluster type. The provider
  reads them.
- **Node pools on their own.** A cluster's pools come from its cluster type.
  You can change a pool's size and metadata only.
- **Node labels and taints.** They're set in the cluster type by its
  operator. A cluster's `tags` don't reach the nodes.
- **The Kubernetes version, directly.** It comes from the cluster type
  version, and `kubernetes_version` is read-only.
  - Pinning `cluster_type` to a later version upgrades the cluster in place,
    one version per step.
  - An earlier version fails the plan, since upgrades only go forward.
  - While `cluster_type` names a version the plan refuses, `terraform
    destroy` fails too. Set `cluster_type` back to the cluster's version, or
    run `terraform destroy -refresh=false`.
  - A cluster left `failed` by an upgrade has to be fixed on the platform
    before it can be upgraded again.
