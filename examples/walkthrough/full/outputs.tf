# What it made, and how to reach it. Each output with a `cond ? {...} : null`
# value is null when its build switch is off.

output "project" {
  value = {
    id           = local.project
    organization = local.organization
    from         = var.build.own_project ? "created here, deleted last on destroy" : "already there"
  }
}

output "cluster" {
  value = {
    id                 = k0rdent_kubernetes_cluster.this.id
    state              = k0rdent_kubernetes_cluster.this.state
    kubernetes_version = k0rdent_kubernetes_cluster.this.kubernetes_version
    nodes              = k0rdent_kubernetes_cluster.this.node_count
    worker_pools       = { for id, p in local.cluster_node_pool : id => p.node_count }
    vpc                = "${k0rdent_kubernetes_cluster.this.vpc} on ${coalesce(data.k0rdent_vpc.owned["cluster"].backend, "no backend reported")}"
    security_groups    = k0rdent_kubernetes_cluster.this.security_groups
    ssh_users          = [for u in k0rdent_kubernetes_cluster.this.ssh_access : u.username]
    on_destroy_busy    = local.on_destroy_busy
    tags               = k0rdent_kubernetes_cluster.this.tags_all
  }

  # Fail if a VPC this made isn't in the project's list.
  precondition {
    condition     = alltrue([for o in values(local.owners) : contains(data.k0rdent_vpcs.all.ids, o.vpc)])
    error_message = "A VPC this made isn't in project ${local.project}'s list of VPCs."
  }
}

output "cluster_type" {
  value = {
    id     = var.cluster_type
    in_use = var.cluster_type_version != "" ? "version ${var.cluster_type_version}, pinned" : "version ${data.k0rdent_cluster_type.chosen.version}, the current one"
    # Any of these can go in cluster_type_version. A later one upgrades the
    # cluster in place.
    versions = {
      for v in data.k0rdent_cluster_type_versions.chosen.versions :
      v.version_id => "version ${v.version}, Kubernetes ${coalesce(v.kubernetes_version, "not given")}"
    }
  }
}

# The security group filters node interfaces, not the API address. Test it on
# a node's SSH port: one nc command per host address.
output "kubernetes_api" {
  value = {
    endpoint = coalesce(data.k0rdent_kubernetes_cluster.this.api_endpoint, "not reported yet")
    filtered = "No. The security group doesn't filter the Kubernetes API address."
    test_the_group = var.build.security_group ? concat(
      var.build.instance_group ? flatten([
        for i in data.k0rdent_instances.hosts[0].instances : [
          for n in i.network_interfaces : [for ip in n.ip_addresses : "nc -vz -w 5 ${ip} 22  # ${i.hostname}"]
        ]
      ]) : [],
      [
        "For the cluster's nodes, apply nodes/ for their addresses, then: nc -vz -w 5 <node address> 22",
        "Run it from inside allowed_cidrs (${join(", ", var.allowed_cidrs)}), then from outside. Inside connects, outside times out.",
        "A change to the group can take a few minutes to reach the nodes.",
      ],
    ) : []
  }
}

output "ssh" {
  value = {
    cluster = "Apply nodes/ with prefix ${var.prefix}, region ${var.region} and project ${local.project}. It lists ${k0rdent_kubernetes_cluster.this.id}'s nodes with an ssh command for each, as ${join(" or ", keys(var.ssh_users))}."
    hosts = var.build.instance_group ? flatten([
      for i in data.k0rdent_instances.hosts[0].instances : [
        for n in i.network_interfaces : [
          for ip in n.ip_addresses : "ssh -i <your private key> ${keys(var.ssh_users)[0]}@${ip}  # ${i.hostname}"
        ]
      ]
    ]) : []
  }
}

output "network" {
  value = var.build.network ? {
    network = "${k0rdent_network.this[0].id}: ${k0rdent_network.this[0].type}, VLAN ${k0rdent_network.this[0].evpn.vlan_id_resolved}, ${k0rdent_network.this[0].state}"
    pool    = "${local.pool.id}: ${join(", ", local.pool.prefixes)}, not routable, /${local.pool.min_prefix_length} to /${local.pool.max_prefix_length}"
    subnets = {
      for purpose, s in k0rdent_subnet.this :
      purpose => "${s.cidr}, asked for /${var.subnets[purpose]}, given /${s.prefix_length}, ${s.state}"
    }
  } : null
}

output "filtering" {
  value = var.build.security_group ? {
    group = k0rdent_security_group.this[0].id
    rules = concat(
      [for r in k0rdent_security_group.this[0].ingress : "in:  ${r.action} ${r.protocol}${r.destination_port_range == null ? "" : " ${r.destination_port_range}"} from ${r.source_prefix} (${r.name})"],
      [for r in k0rdent_security_group.this[0].egress : "out: ${r.action} ${r.protocol} to ${r.destination_prefix} (${r.name})"],
    )
    vpc_bindings = { for owner, b in k0rdent_vpc_security_groups.this : "${owner} VPC ${b.vpc}" => b.attached }
    # Each group that reaches each owner, in the order it applies, and what
    # bound it.
    effective = {
      for owner, e in data.k0rdent_effective_security_groups.owned : owner => {
        for v in e.vpcs :
        v.id => [for g in v.security_groups : "${g.id} (bound by ${g.source} ${g.source_id}, ${g.rule_count} rules)"]
      }
    }
  } : null
}

output "webhook" {
  value = var.build.webhook ? {
    id             = k0rdent_webhook_subscription.this[0].id
    url            = k0rdent_webhook_subscription.this[0].url
    events         = k0rdent_webhook_subscription.this[0].event_types
    active         = k0rdent_webhook_subscription.this[0].is_active
    secret_version = k0rdent_webhook_subscription.this[0].secret_version
    rotate         = "export TF_VAR_webhook_secret=$(openssl rand -hex 32), set webhook_secret_version = ${var.webhook_secret_version + 1} in terraform.tfvars, and apply"
  } : null
}

output "instance_group" {
  value = var.build.instance_group ? {
    id    = k0rdent_instance_group.this[0].id
    state = k0rdent_instance_group.this[0].state
    vpc   = k0rdent_instance_group.this[0].vpc
    hosts = [for i in data.k0rdent_instances.hosts[0].instances : "${i.hostname}: ${i.state}, ${i.health}"]
  } : null
}

output "file_store" {
  value = var.build.file_store ? {
    id       = k0rdent_file_store.this[0].id
    state    = k0rdent_file_store.this[0].state
    capacity = "${k0rdent_file_store.this[0].capacity_gib} GiB of ${k0rdent_file_store.this[0].share_type}"
    mount    = try("${k0rdent_file_store.this[0].preferred_export_location.server}:${k0rdent_file_store.this[0].preferred_export_location.path}, at ${var.file_store.mount_path}", null)
    grants   = { for ref, c in coalesce(k0rdent_file_store.this[0].consumer_status, {}) : ref => c.state }
  } : null
}

output "vpc_peering" {
  value = var.build.vpc_peering ? {
    for key, p in k0rdent_vpc_peering.this : p.id => "${key}, ${p.state}"
  } : null
}

# What the platform reports back, for the scorecard. None of it stops a run.
# concat([0], ...) keeps sum() working on an empty list.
locals {
  over_quota = var.quota_checks ? concat(
    [for k, d in data.k0rdent_project_quota.this[0].dimensions : "${k} in the project" if d.over_limit],
    [for k, d in data.k0rdent_organization_quota.this[0].dimensions : "${k} in the organization" if d.over_limit],
  ) : []

  tags_landed = concat(
    var.build.network ? ["network ${contains(data.k0rdent_networks.tagged.ids, local.name) ? "yes" : "no"}"] : [],
    var.build.security_group ? ["security group ${contains(data.k0rdent_security_groups.tagged.ids, local.name) ? "yes" : "no"}"] : [],
  )

  hosts_ready = var.build.instance_group ? {
    ready   = sum(concat([0], [for p in values(data.k0rdent_instance_group.hosts[0].node_pools) : coalesce(p.ready_count, 0)]))
    desired = sum(concat([0], [for p in values(data.k0rdent_instance_group.hosts[0].node_pools) : coalesce(p.desired_count, 0)]))
  } : null
}

# One line per thing this directory can build: what it made, or the switch
# that turns it on and what that needs.
output "scorecard" {
  value = {
    "project"                       = var.build.own_project ? "made ${local.project}" : "used ${local.project}. build.own_project makes one, with a credential scoped to the organization"
    "quota"                         = !var.quota_checks ? "not read. quota_checks is false" : length(local.over_quota) == 0 ? "within every limit" : "over on ${join(", ", local.over_quota)}"
    "cluster"                       = "made ${k0rdent_kubernetes_cluster.this.id}, ${k0rdent_kubernetes_cluster.this.state}, with ${join(", ", keys(var.ssh_users))} on every node"
    "control plane"                 = coalesce(data.k0rdent_kubernetes_cluster.this.control_plane_state, "not reported")
    "kubeconfig"                    = "${coalesce(data.k0rdent_cluster_kubeconfig.this.status, "no status")}, issued for ${coalesce(data.k0rdent_cluster_kubeconfig.this.issued_for, "no one named")}"
    "network and subnets"           = var.build.network ? format("made %s, with %s", k0rdent_network.this[0].id, join(", ", [for purpose, s in k0rdent_subnet.this : "${purpose} ${s.cidr}"])) : "off. build.network, needs an address pool that isn't routable"
    "tags landed"                   = length(local.tags_landed) > 0 ? join(", ", local.tags_landed) : "nothing tagged to look for"
    "security group on the cluster" = var.build.security_group ? "made ${k0rdent_security_group.this[0].id}, rules: ${length(local.ingress_rules) + length(local.ingress_denies) + length(local.egress_rules)} sent, ${coalesce(k0rdent_security_group.this[0].rule_count, 0)} on the platform" : "off. build.security_group, needs a cluster type whose nodes sit on a nico-backed VPC"
    "security group on the VPCs"    = var.build.vpc_security_groups ? "on the ${join(" and ", keys(k0rdent_vpc_security_groups.this))} ${length(k0rdent_vpc_security_groups.this) > 1 ? "VPCs" : "VPC"}" : "off. build.vpc_security_groups, needs build.security_group and a nico-backed VPC"
    "webhook"                       = var.build.webhook ? "made ${k0rdent_webhook_subscription.this[0].id}, ${k0rdent_webhook_subscription.this[0].is_active ? "active" : "not active"}, subscriptions to this URL: ${length(data.k0rdent_webhook_subscriptions.same_url[0].subscriptions)}" : "off. build.webhook, needs webhook_url and TF_VAR_webhook_secret"
    "instance group"                = var.build.instance_group ? "made ${k0rdent_instance_group.this[0].id}, ${k0rdent_instance_group.this[0].state}, ${local.hosts_ready.ready} of ${local.hosts_ready.desired} hosts ready" : "off. build.instance_group, needs a free host"
    "file store"                    = var.build.file_store ? "made ${k0rdent_file_store.this[0].id}, ${k0rdent_file_store.this[0].state}, ${data.k0rdent_file_store.this[0].reconciling ? "still settling" : "settled"}" : "off. build.file_store, needs a share type in the region"
    "vpc peering"                   = var.build.vpc_peering ? "both sides declared, ${join(" and ", [for p in values(k0rdent_vpc_peering.this) : p.state])}, the cluster's side ${join(", ", [for p in data.k0rdent_vpc_peerings.cluster[0].peerings : p.state])}" : "off. build.vpc_peering, needs build.instance_group"
  }
}
