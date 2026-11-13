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

The token request has a limit of its own. Each provider process asks for one
access token and reuses it until shortly before it expires. A `terraform plan`
takes one token, and so does an `apply` with nothing to change. An `apply`
that changes something, or a `destroy`, takes two, because Terraform starts
the provider again to make the change. A run that outlasts its token takes
another. By default the platform gives a credential 10 tokens a minute, and a
refused request counts too, so every run that uses one credential at the same
time shares those 10. Give each pipeline that runs at the same time its own
credential. A 429 on the token request says which limit it met, the
credential's or the gateway's for one client address, and how long the
platform asked to wait if it named a wait.

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

## Terraform 1.16.0 to 1.16.4

A ctrl-C during an apply that's creating two or more resources can crash
Terraform 1.16.0 to 1.16.4 and leave the state file empty. What the apply
created stays on the platform, and neither the next apply nor a destroy knows
about it. It's a Terraform bug, fixed in 1.16.5
([hashicorp/terraform#39287](https://github.com/hashicorp/terraform/pull/39287)).
Use 1.16.5 or later, or OpenTofu, which doesn't have it.

If it already happened, [import](guides/import.md) each resource the run
created, then keep it or destroy it.

## A refused plan stops `terraform destroy` too

`terraform destroy` plans as if applying before it plans the delete, so a
refusal in that plan stops the destroy as well. These are still refused at
plan:

- A smaller `capacity_gib` on a file store
- A `region` that isn't the project's

Set the configuration back to what's applied, or run
`terraform destroy -refresh=false`, which destroys from state as it is.

A cluster's `cluster_type` version change is made in place, so its refusal
waits for the apply and doesn't stop a destroy.

## A refused replacement fails the apply, not the plan

A change that would replace a cluster, instance group, file store, subnet,
network or project, without `allow_replace = true`, doesn't fail the plan, so
`terraform destroy` still works. What you see instead:

- The plan shows `~ update in-place` with a warning that names the attribute,
  where Terraform would otherwise show `-/+ must be replaced`.
- `terraform plan -detailed-exitcode` exits 2, a plan with changes.
- The apply refuses that resource's update before it sends any request, so
  none of its changes in the plan are made. The rest of the plan still
  applies.
- A resource that depends on the refused change, such as a `terraform_data`
  whose `triggers_replace` reads the changed attribute, is replaced as the
  plan shows: Terraform destroys it before the apply refuses, and doesn't
  create it again until the change is reverted or permitted. When the new
  value is known at plan, computed values the change would have left unknown
  keep their current values, so a resource that reads only those isn't
  replaced.
- A new value known only after apply is refused only if it differs from the
  current one. If it doesn't, the apply makes the rest of the change in
  place.
- `terraform apply -replace` replaces the resource anyway, as it asks, with
  the change in it. If it does, a file store's plan has already warned which
  consumers lose access, and a cluster's that a running operation is
  cancelled.
- An `allow_replace` known only after apply, such as one read from a resource
  the same apply creates, can't permit the replacement. If it turns out
  `true`, Terraform stops the apply with "Provider produced inconsistent final
  plan" before it changes the resource. Set `allow_replace` to a value known
  at plan time.

Set `allow_replace = true` to let the replacement go ahead, or set the
attribute back to its current value.

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
- **Service accounts, their keys, and the roles they hold.** The platform
  lets only a signed-in person create, change or delete them, and the
  provider signs in as a service account or with an API key. The
  `k0rdent_service_accounts` and `k0rdent_service_account_credentials` data
  sources can read them.
- **Node pools on their own.** A cluster's pools come from its cluster type.
  You can change a pool's size and metadata only.
- **Node labels and taints.** They're set in the cluster type by its
  operator. A cluster's `tags` don't reach the nodes.
- **The Kubernetes version, directly.** It comes from the cluster type
  version, and `kubernetes_version` is read-only.
  - Pinning `cluster_type` to a later version upgrades the cluster in place,
    one version per step.
  - An earlier version plans with a warning and fails the apply before it
    sends anything, since upgrades only go forward. So does any other
    version change the cluster can't take. `terraform destroy` still works.
  - A cluster left `failed` by an upgrade has to be fixed on the platform
    before it can be upgraded again.
