---
page_title: "Hardcoded values: known gaps"
subcategory: ""
description: |-
  The values a configuration still has to write as literals, because no read a
  tenant credential can make supplies them, and the side that has to change.
---

# Hardcoded values: known gaps

Most values a configuration needs can be read with a data source, such as
`k0rdent_regions`, `k0rdent_cluster_types`, `k0rdent_cluster_type_versions`,
`k0rdent_organizations` and `k0rdent_instances`. The values on this page can't
be read by a tenant, so you write them as literals, from your operator or the
console.

Each row names the side that has to change before you can read it:

- **API**: the k0rdent AI API has to add a read.
- **Provider**: the API has the read, and this provider has to wrap it.
- **Product**: a decision about who may read what.
- **Platform**: the running platform or its seed data.

## The attributes

| Attribute | Kind | Value a tenant cannot read | Why | Owner | Proof |
| --- | --- | --- | --- | --- | --- |
| `k0rdent_kubernetes_cluster.cluster_type` | resource | The hardware, GPU model and image behind the ClusterType's machine types | Not readable by a tenant | API | [Machine types](#machine-types-gpu-types-and-images) |
| `k0rdent_kubernetes_cluster.node_pool` | resource | The hardware behind each pool's machine type | Not readable by a tenant | API | [Machine types](#machine-types-gpu-types-and-images) |
| `k0rdent_instance_group.cluster_type` | resource | The hardware, GPU model and image behind the ClusterType's machine types | Not readable by a tenant | API | [Machine types](#machine-types-gpu-types-and-images) |
| `k0rdent_instance_group.node_pool` | resource | The hardware behind each pool's machine type | Not readable by a tenant | API | [Machine types](#machine-types-gpu-types-and-images) |
| `k0rdent_cluster_type.node_pools.machine_type` | data source | What the machine type it names is | Not readable by a tenant | API | [Machine types](#machine-types-gpu-types-and-images) |
| `k0rdent_cluster_type_version.node_pools.machine_type` | data source | What the machine type it names is | Not readable by a tenant | API | [Machine types](#machine-types-gpu-types-and-images) |
| `k0rdent_instance_group.node_pools.machine_type` | data source | What the machine type it names is | Not readable by a tenant | API | [Machine types](#machine-types-gpu-types-and-images) |
| `k0rdent_network.infiniband.partition` | resource | The InfiniBand partitions an operator defined in the region | No API | API | None yet |
| `k0rdent_network.nvlink.partition` | resource | The NVLink logical partitions an operator defined in the region | No API | API | None yet |
| `k0rdent_project.organization_id` | resource | The organization, while it holds no project the credential can see | An API key may list no organizations | API | None yet |
| `k0rdent_webhook_subscription.event_types` | resource | The event types the platform emits, which its event catalog lists | Not readable by a tenant | Product | None yet |
| `k0rdent_service_accounts.scope_id` | data source | A folder's id, for `scope = "folder"`. A project's or an organization's is read through `k0rdent_projects` | No API | API | None yet |
| `k0rdent_service_account_credentials.scope_id` | data source | A folder's id, for `scope = "folder"`. A project's or an organization's is read through `k0rdent_projects` | No API | API | None yet |

## Machine types, GPU types and images

You never set a machine type or image directly. You pick a ClusterType, and
each of its node pools names a machine type. You can read the machine type's
name, but not what hardware or image it is.

Ask your operator which ClusterType and pool have the hardware you need. Then
write those ids, or filter `node_pools` by machine type name.

## Fabric partitions

- An InfiniBand network on the `nico` backend needs `infiniband.partition`.
- An NVLink network may set `nvlink.partition`, or leave it out and let the
  fabric pick one.

Your operator defines the partitions, and no API lists them, so ask them.

## An organization with no project yet

`k0rdent_project` needs `organization_id`, and the provider can't create an
organization. `k0rdent_projects` shows the organization of each project you
can see. With an API key the organization list may be empty, so the first
project in an organization needs the id as a literal. Use
`k0rdent_organization` to check it's `active` before creating the project.

## Event types

`k0rdent_webhook_subscription` takes `event_types`, such as
`io.k0rdent.workflow.cluster.create.v1`. You can't read the event catalog, so
get names from the platform documentation or your operator. The plan warns
about a name that doesn't look like one. Leave `event_types` out to get every
event.

A name in the right form can still match nothing:

- `io.k0rdent.cluster.ready.v1` is sent when a degraded cluster is ready
  again, not when a create finishes. A finished create is
  `io.k0rdent.workflow.cluster.create.v1`, and a failed one is
  `io.k0rdent.workflow.cluster.create.failed.v1`.
- `io.k0rdent.cluster.failed.v1` is retired, so nothing is delivered for it.

To see the names your platform sends, subscribe once with `event_types` left
out and read the `type` of each event your endpoint receives.

## Now read through a data source

- **A machine's interface on a file store grant.** A grant to an instance
  group can narrow the address to one interface, by `mac_address` (Ethernet)
  or `guid` (InfiniBand). Read them from `network_interfaces` on
  `k0rdent_instances` or `k0rdent_instance`. Or leave both out:
  `network.type` alone grants every member's interfaces on that fabric.

## Values no attribute takes

| Value | Why | Owner | Proof |
| --- | --- | --- | --- |
| A region's zones | No API | API | None yet |

No resource takes a zone, so you can't place anything by zone.
