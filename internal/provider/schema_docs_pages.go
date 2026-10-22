package provider

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Page sections the schema can't carry: a Timeouts section on every page
// whose surface has a timeouts block, and a Related section on every page.
//
// The documentation generator renders a schema's description above the
// examples and its attributes below them, and has nowhere for either of
// these. internal/tools/docsections writes them into the generated pages from
// PageSections, after the generator runs, so they come from the same code as
// everything else on the page and `make docs-check` sees them go stale.

// relatedLink is one entry in a page's Related section.
type relatedLink struct {
	// page is the target's page path, as on docSurface: "resources/network".
	page string
	// why says what the target is to this surface, as a clause after the
	// link: "the network the subnet is allocated in."
	why string
}

// relatedPages are the links a reader of one page wants and that the names
// alone don't give. A resource's data source and list resource of the same
// name, and a data source's singular or plural sibling, are linked without
// being listed here
// (counterpartLinks). TestEveryPageLinksItsRelated fails on a surface with no
// link at all, and on a link to a page nothing registers.
var relatedPages = map[string][]relatedLink{
	"resources/kubernetes_cluster": {
		{"data-sources/cluster_types", "lists the ClusterTypes whose `self_link` `cluster_type` takes."},
		{"data-sources/cluster_type", "reads one ClusterType, whose `self_link` `cluster_type` takes."},
		{"data-sources/cluster_type_version", "reads one version, whose `self_link` pins the cluster to it."},
		{"data-sources/cluster_type_versions", "lists the versions a cluster can be pinned to or upgraded through."},
		{"resources/security_group", "defines a group `security_groups` lists."},
		{"ephemeral-resources/cluster_kubeconfig", "fetches the credential for the cluster's API without writing it to state."},
		{"data-sources/effective_security_groups", "reports what actually filters the cluster's interfaces."},
		{"resources/vpc_peering", "peers the VPC the cluster materialized with another."},
		{"actions/cluster_cancel_operation", "cancels the operation the cluster is running, and keeps the cluster."},
	},
	"resources/instance_group": {
		{"data-sources/cluster_types", "lists the ClusterTypes whose `self_link` `cluster_type` takes."},
		{"data-sources/cluster_type", "reads one ClusterType, whose `self_link` `cluster_type` takes."},
		{"data-sources/instances", "lists the hosts the group holds."},
		{"resources/file_store", "mounts storage on every host in the group."},
		{"data-sources/effective_security_groups", "reports what actually filters the group's interfaces."},
	},
	"resources/file_store": {
		{"data-sources/share_types", "lists the classes `share_type` takes."},
		{"resources/instance_group", "a consumer that grants every host in the group."},
		{"data-sources/instances", "reads the addresses a narrower grant names."},
	},
	"resources/network": {
		{"resources/subnet", "allocates a CIDR inside the network."},
	},
	"resources/subnet": {
		{"resources/network", "the network the subnet is allocated in."},
		{"data-sources/address_pools", "lists the pools whose `self_link` `address_pool` takes."},
	},
	"resources/project": {
		{"data-sources/organizations", "lists the organizations `organization_id` takes."},
		{"data-sources/project_quota", "reports what the project may still use."},
	},
	"resources/security_group": {
		{"resources/kubernetes_cluster", "binds groups to a cluster through `security_groups`."},
		{"resources/vpc_security_groups", "binds groups to a VPC."},
		{"data-sources/effective_security_groups", "reports what actually filters an object once every binding is merged."},
	},
	"resources/vpc_peering": {
		{"data-sources/vpc", "reads a VPC's `self_link`, which `remote_vpc` takes for one in another project."},
		{"data-sources/vpcs", "lists the VPCs `vpc` and `remote_vpc` name."},
		{"resources/kubernetes_cluster", "materializes the VPC a peering is declared under."},
		{"resources/instance_group", "materializes a VPC too, whose `vpc` a peering in this project takes as `remote_vpc`."},
	},
	"resources/vpc_security_groups": {
		{"resources/security_group", "defines a group `security_groups` lists."},
		{"resources/instance_group", "brings a VPC of its own, named by its `vpc`."},
		{"data-sources/vpcs", "lists the VPCs `vpc` takes."},
		{"data-sources/effective_security_groups", "reports what actually filters a VPC's interfaces."},
	},

	"data-sources/address_pool":  {{"resources/subnet", "takes the pool's `self_link` in `address_pool`."}},
	"data-sources/address_pools": {{"resources/subnet", "takes a pool's `self_link` in `address_pool`."}},
	"data-sources/cluster_kubeconfig": {
		{"ephemeral-resources/cluster_kubeconfig", "fetches the kubeconfig itself, never written to state."},
		{"resources/kubernetes_cluster", "manages the cluster the kubeconfig reaches."},
	},
	"data-sources/cluster_type": {
		{"resources/kubernetes_cluster", "takes the ClusterType's `self_link` in `cluster_type`."},
		{"resources/instance_group", "takes the ClusterType's `self_link` in `cluster_type`."},
		{"data-sources/cluster_type_versions", "lists every version of it."},
	},
	"data-sources/cluster_types": {
		{"resources/kubernetes_cluster", "takes a ClusterType's `self_link` in `cluster_type`."},
		{"resources/instance_group", "takes a ClusterType's `self_link` in `cluster_type`."},
	},
	"data-sources/cluster_type_version": {
		{"resources/kubernetes_cluster", "is pinned to a version by the version's `self_link`."},
		{"data-sources/cluster_type", "reads the ClusterType at its newest version."},
	},
	"data-sources/cluster_type_versions": {
		{"resources/kubernetes_cluster", "is pinned to a version by the version's `self_link`."},
		{"data-sources/cluster_type", "reads the ClusterType at its newest version."},
	},
	"data-sources/effective_security_groups": {
		{"resources/security_group", "defines a group."},
		{"resources/kubernetes_cluster", "binds groups to a cluster."},
		{"resources/vpc_security_groups", "binds groups to a VPC."},
	},
	"data-sources/instance": {
		{"resources/instance_group", "manages the group the host belongs to."},
		{"resources/file_store", "grants a host by the addresses read here."},
	},
	"data-sources/instances": {
		{"resources/instance_group", "manages the groups the hosts belong to."},
		{"resources/file_store", "grants hosts by the addresses read here."},
	},
	"data-sources/instance_group": {
		{"data-sources/instances", "lists the hosts the group holds."},
	},
	"data-sources/kubernetes_cluster": {
		{"ephemeral-resources/cluster_kubeconfig", "fetches the credential for the cluster's API."},
		{"actions/cluster_cancel_operation", "cancels the operation `operation.execution_id` names."},
	},
	"data-sources/network": {{"resources/subnet", "allocates a CIDR inside a network."}},
	"data-sources/networks": {
		{"resources/subnet", "allocates a CIDR inside a network."},
		{"resources/kubernetes_cluster", "materializes networks of its own, which carry a `cluster_id`."},
	},
	"data-sources/organization": {
		{"resources/project", "takes an organization's id in `organization_id`."},
	},
	"data-sources/organizations": {
		{"resources/project", "takes an organization's id in `organization_id`."},
	},
	"data-sources/project_quota": {
		{"resources/project", "manages the project the quota applies to."},
		{"data-sources/tenant_quota", "reports the organization's own limits."},
	},
	"data-sources/projects": {
		{"data-sources/organizations", "lists the organizations projects live in."},
	},
	"data-sources/roles": {
		{"data-sources/service_accounts", "reports the role ids each account holds in `roles`."},
	},
	"data-sources/service_account": {
		{"data-sources/service_accounts", "lists the accounts whose `id` this reads."},
		{"data-sources/service_account_credentials", "lists the account's credentials and when each expires."},
	},
	"data-sources/service_accounts": {
		{"data-sources/roles", "explains the role ids in each account's `roles`."},
		{"data-sources/service_account_credentials", "lists each account's credentials and when each expires."},
	},
	"data-sources/service_account_credentials": {
		{"data-sources/service_accounts", "lists the accounts whose `id` `service_account_id` takes."},
	},
	"data-sources/regions": {
		{"data-sources/cluster_types", "lists what a region offers to build clusters from."},
		{"data-sources/address_pools", "lists the pools a region allocates subnets from."},
	},
	"data-sources/security_groups": {
		{"data-sources/effective_security_groups", "reports what actually filters an object once every binding is merged."},
	},
	"data-sources/share_type":  {{"resources/file_store", "takes a class's id in `share_type`."}},
	"data-sources/share_types": {{"resources/file_store", "takes a class's id in `share_type`."}},
	"data-sources/subnets": {
		{"data-sources/networks", "lists the networks whose subnets this reads."},
		{"data-sources/address_pools", "lists the pools subnets are allocated from."},
	},
	"data-sources/tenant_quota": {
		{"data-sources/project_quota", "reports one project's share of it."},
		{"resources/kubernetes_cluster", "sizes a node pool, which the quota caps."},
		{"data-sources/cluster_type", "reads the ClusterType a cluster is built from."},
		{"resources/project", "creates projects inside an organization."},
	},
	"data-sources/vpc": {
		{"resources/vpc_peering", "peers the VPC with another."},
		{"resources/vpc_security_groups", "binds security groups to the VPC."},
	},
	"data-sources/vpcs": {
		{"resources/vpc_peering", "peers a VPC with another."},
		{"resources/vpc_security_groups", "binds security groups to a VPC."},
		{"data-sources/effective_security_groups", "reports what actually filters a VPC's interfaces."},
	},
	"data-sources/vpc_peerings": {
		{"data-sources/vpcs", "lists the VPCs a peering joins."},
	},

	"actions/cluster_cancel_operation": {
		{"resources/kubernetes_cluster", "manages the cluster whose operation this cancels."},
		{"data-sources/kubernetes_cluster", "reads the operation the cluster is running, whose `execution_id` `execution_id` takes."},
	},

	"ephemeral-resources/cluster_kubeconfig": {
		{"resources/kubernetes_cluster", "manages the cluster the kubeconfig reaches."},
		{"data-sources/cluster_kubeconfig", "reads the kubeconfig's metadata, which is safe to keep in state."},
	},
}

// counterpartLinks are the links the names give: a resource and the data
// source and list resource of the same name, and a data source and its
// singular or plural.
func counterpartLinks(page string, registered map[string]docSurface) []relatedLink {
	dir, short, _ := strings.Cut(page, "/")
	var out []relatedLink
	switch dir {
	case "resources":
		if _, ok := registered["data-sources/"+short]; ok {
			out = append(out, relatedLink{"data-sources/" + short, "reads one this configuration doesn't manage."})
		}
		if _, ok := registered["list-resources/"+short]; ok {
			out = append(out, relatedLink{"list-resources/" + short, "finds every one, for `terraform query` to adopt."})
		}
	case "list-resources":
		if _, ok := registered["resources/"+short]; ok {
			out = append(out, relatedLink{"resources/" + short, "manages one, and is what each result is adopted as."})
		}
	case "data-sources":
		if _, ok := registered["resources/"+short]; ok {
			out = append(out, relatedLink{"resources/" + short, "manages one."})
		}
		if _, ok := registered["data-sources/"+short+"s"]; ok {
			out = append(out, relatedLink{"data-sources/" + short + "s", "lists every one."})
		}
		if singular, ok := strings.CutSuffix(short, "s"); ok {
			if _, ok := registered["data-sources/"+singular]; ok {
				out = append(out, relatedLink{"data-sources/" + singular, "reads one by id."})
			}
		}
	}
	return out
}

// relatedLinks is a page's whole Related list: its counterparts, then the
// curated links, each target once.
func relatedLinks(page string, registered map[string]docSurface) []relatedLink {
	var out []relatedLink
	seen := map[string]bool{page: true}
	for _, l := range append(counterpartLinks(page, registered), relatedPages[page]...) {
		if seen[l.page] {
			continue
		}
		seen[l.page] = true
		out = append(out, l)
	}
	return out
}

// kindOfPage is the noun a Related entry shows after its link.
func kindOfPage(page string) string {
	for kind, dir := range pageDirs {
		if strings.HasPrefix(page, dir+"/") {
			return kind
		}
	}
	return ""
}

// PageSections is what internal/tools/docsections adds to each generated
// page, keyed by the page's path under docs/ without ".md". Each value is
// markdown made of whole `## ` sections.
func PageSections(ctx context.Context) map[string]string {
	all := docSurfaces(ctx)
	registered := map[string]docSurface{}
	for _, s := range all {
		if s.page != "" {
			registered[s.page] = s
		}
	}
	out := map[string]string{}
	for _, s := range all {
		if s.page == "" {
			continue
		}
		var b strings.Builder
		if t := timeoutsSection(s); t != "" {
			b.WriteString(t)
		}
		if links := relatedLinks(s.page, registered); len(links) > 0 {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("## Related\n\n")
			for _, l := range links {
				target := registered[l.page]
				fmt.Fprintf(&b, "- [`%s`](../%s.md) %s: %s\n", target.name, l.page, kindOfPage(l.page), l.why)
			}
		}
		out[s.page] = b.String()
	}
	return out
}

// timeoutOrder is the order a reader expects the operations in.
var timeoutOrder = []string{"create", "read", "update", "delete", "open", "invoke"}

// timeoutOperationsOf lists the operations a surface's timeouts block takes,
// read from the block itself.
func timeoutOperationsOf(s docSurface) []string {
	if !s.blocks.IsValid() || s.blocks.Kind() != reflect.Map || s.blocks.IsNil() {
		return nil
	}
	block := concrete(s.blocks.MapIndex(reflect.ValueOf("timeouts")))
	if !block.IsValid() {
		return nil
	}
	attrs := block.FieldByName("Attributes")
	if !attrs.IsValid() || attrs.Kind() != reflect.Map {
		return nil
	}
	var ops []string
	for _, op := range timeoutOrder {
		if attrs.MapIndex(reflect.ValueOf(op)).IsValid() {
			ops = append(ops, op)
		}
	}
	return ops
}

// longTimeoutOperations are the operations of a surface whose default is
// DefaultLongOperationTimeout, read from the block hardwareTimeouts made.
func longTimeoutOperations(s docSurface) map[string]bool {
	if !s.blocks.IsValid() || s.blocks.Kind() != reflect.Map || s.blocks.IsNil() {
		return nil
	}
	block := concrete(s.blocks.MapIndex(reflect.ValueOf("timeouts")))
	if !block.IsValid() {
		return nil
	}
	attrs := block.FieldByName("Attributes")
	if !attrs.IsValid() || attrs.Kind() != reflect.Map {
		return nil
	}
	long := map[string]bool{}
	for _, k := range attrs.MapKeys() {
		op := concrete(attrs.MapIndex(k))
		if !op.IsValid() {
			continue
		}
		if d := op.FieldByName("MarkdownDescription"); d.IsValid() &&
			strings.Contains(d.String(), longTimeoutDefaultSentence()) {
			long[k.String()] = true
		}
	}
	return long
}

// timeoutsSection is the Timeouts section of a page whose surface has a
// timeouts block: each operation with its default, and the block written out.
func timeoutsSection(s docSurface) string {
	ops := timeoutOperationsOf(s)
	if len(ops) == 0 {
		return ""
	}
	def := shortDuration(DefaultOperationTimeout)
	long := longTimeoutOperations(s)
	var b strings.Builder
	b.WriteString("## Timeouts\n\n")
	b.WriteString("The `timeouts` block sets how long each operation may take before it fails:\n\n")
	for _, op := range ops {
		d := def
		if long[op] {
			d = shortDuration(DefaultLongOperationTimeout)
		}
		fmt.Fprintf(&b, "- `%s` (default `%s`). %s\n", op, d, timeoutOperations[op])
	}
	if len(long) > 0 {
		fmt.Fprintf(&b, "\nA create or a delete builds or frees hardware, which can take more than 2 "+
			"hours, so it defaults to `%s`. Every other default is "+
			"the provider's `operation_timeout`, which is `%s` unless the provider block or `%s` sets "+
			"it. Once it's set, it's every operation's default, these two included. ",
			shortDuration(DefaultLongOperationTimeout), def, EnvVar(envOperationTimeout))
	} else {
		fmt.Fprintf(&b, "\nEach default is the provider's `operation_timeout`, which is `%s` unless the "+
			"provider block or `%s` sets it. An operation the block leaves out takes that value, so "+
			"raising `operation_timeout` raises all of them at once. ",
			def, EnvVar(envOperationTimeout))
	}
	b.WriteString("A value of zero or less is refused at plan time rather than read as no limit.\n\n")
	if s.kind == "resource" && slices.Contains(ops, "create") {
		b.WriteString("A wait that runs out, or a run that is interrupted, stops nothing on the platform. " +
			"A create that stops waiting fails, and Terraform taints the object: run `terraform untaint` on it " +
			"and then `terraform apply`, which waits for the same object. A delete that stops waiting fails " +
			"and stays in state, and the next `terraform destroy` waits for the same delete without sending " +
			"another.\n\n")
	}
	block, indent := "resource", "  "
	switch s.kind {
	case "ephemeral resource":
		block = "ephemeral"
	case "action":
		// An action's arguments, its timeouts block among them, go in its
		// `config` block.
		block, indent = "action", "    "
	}
	fmt.Fprintf(&b, "```terraform\n%s %q \"example\" {\n", block, s.name)
	if s.kind == "action" {
		b.WriteString("  config {\n")
	}
	fmt.Fprintf(&b, "%s# ...\n\n%stimeouts {\n", indent, indent)
	width := 0
	for _, op := range ops {
		width = max(width, len(op))
	}
	for _, op := range ops {
		fmt.Fprintf(&b, "%s  %-*s = \"45m\"\n", indent, width, op)
	}
	fmt.Fprintf(&b, "%s}\n", indent)
	if s.kind == "action" {
		b.WriteString("  }\n")
	}
	b.WriteString("}\n```\n")
	return b.String()
}

// RelatedGaps lists every page with no Related link, and every link to a page
// nothing registers.
func RelatedGaps(ctx context.Context) []string {
	registered := map[string]docSurface{}
	for _, s := range docSurfaces(ctx) {
		if s.page != "" {
			registered[s.page] = s
		}
	}
	var gaps []string
	for page := range relatedPages {
		if _, ok := registered[page]; !ok {
			gaps = append(gaps, fmt.Sprintf("%s: listed in relatedPages, and nothing registers it", page))
		}
	}
	for page := range registered {
		links := relatedLinks(page, registered)
		if len(links) == 0 {
			gaps = append(gaps, page+": links to nothing related")
		}
		for _, l := range links {
			if _, ok := registered[l.page]; !ok {
				gaps = append(gaps, fmt.Sprintf("%s: links to %s, and nothing registers it", page, l.page))
			}
		}
	}
	slices.Sort(gaps)
	return gaps
}
