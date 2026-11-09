package acceptance

import (
	"fmt"
	"regexp"
)

// The rows for each kind, written from its resource doc. A row's rule is the
// doc's, and the comment above a row says where the doc states it when it
// isn't the "What replaces it" paragraph. See TestReplacementsFollowTheDocumentedRules.

const invKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICalTR9icsm8WarWXVQxwy8TRDjP7bQ/eoJdFARMvUlQ inv-replace"

var (
	// The cluster's refusal of an earlier version (known-limitations: "An
	// earlier version fails the plan").
	earlierVersion = regexp.MustCompile(`Cannot move the cluster to a different version of its ClusterType`)
	// A region its project isn't in, refused even with allow_replace, since a
	// project is locked to one region (known-limitations: "A refused plan
	// stops `terraform destroy` too", and the region attribute: a resource
	// can't move between regions). allow_replace permits a region change only
	// with a project in that region.
	regionNotProjects = regexp.MustCompile(`This resource's region isn't its project's`)
	shrink            = regexp.MustCompile(`A file store cannot be shrunk`)
	cantHoldProject   = regexp.MustCompile(`This region can't hold a project`)
)

func nothing(b block) block { return b }

// The rows every project-scoped kind has: nothing, a new id and the two
// scope moves.
func scopeRows(idLine string) []invChange {
	return []invChange{
		{label: "nothing", rule: unchanged, edit: nothing},
		{label: "id", attr: "id", rule: replaces, edit: set("id", idLine)},
		{label: "project", attr: "project", rule: replaces, edit: set("project", `project = "example"`)},
		{label: "region", attr: "region", rule: refused, refusal: regionNotProjects, onCreate: true,
			edit: set("region", `region = "us-elsewhere-1"`), silent: "refused whatever allow_replace says"},
	}
}

func q(name, value string) string { return fmt.Sprintf("%s = %q", name, value) }

func noPrelude(string) string { return "" }

func scopedImport(name string) string { return "us-texas-1/demo/" + name }

func invKinds() []invKind {
	return []invKind{
		{
			unexercised: map[string]string{"region": "the fake's projects are all in its one region, so a move is refused as a region its project isn't in, a row of its own"},
			typ:         clusterType, noun: "cluster", guarded: true,
			replacing: []string{"cluster_type", "id", "project", "region", "ssh_access"},
			prelude:   noPrelude,
			base: func(name string) block {
				return block{
					"id":           q("id", name),
					"cluster_type": q("cluster_type", clusterTypeV2),
					"display_name": q("display_name", "inv"),
					"node_pool":    `node_pool = { "control-plane" = { node_count = 1 }, "gpu-workers" = { node_count = 2 } }`,
				}
			},
			importID: scopedImport,
			pathOf:   clusterAPIPath,
			changes: append(scopeRows(`id = "acc-inv-renamed"`),
				invChange{label: "display_name", rule: changesInPlace, edit: set("display_name", q("display_name", "inv-renamed"))},
				// known-limitations: pinning a later version upgrades in place.
				// cluster_type doc, and on_failed's: while the platform reports
				// the cluster `failed`, a plan that pins another version of its
				// ClusterType fails, whatever on_failed and allow_replace say.
				invChange{label: "cluster_type, the next version", rule: changesInPlace, whenFailed: earlierVersion,
					edit: set("cluster_type", q("cluster_type", clusterTypeRef+"/versions/33333333-3333-3333-3333-333333333333"))},
				// known-limitations: an earlier version fails the plan.
				invChange{label: "cluster_type, an earlier version", attr: "cluster_type", rule: refused, refusal: earlierVersion,
					edit: set("cluster_type", q("cluster_type", clusterTypeRef+"/versions/11111111-1111-1111-1111-111111111111"))},
				// cluster_type doc: a path and an absolute URL are both accepted,
				// and only another ClusterType replaces.
				invChange{label: "cluster_type, the same version as a URL", rule: changesInPlace,
					edit: func(b block) block { return b.with("cluster_type", q("cluster_type", endpoint+clusterTypeV2)) }},
				invChange{label: "cluster_type, another ClusterType", attr: "cluster_type", rule: replaces,
					edit: func(b block) block {
						return b.with("cluster_type", q("cluster_type", "/v1/regions/us-texas-1/compute/cluster-types/cpu-general")).
							with("node_pool", "")
					}},
				invChange{label: "ssh_access, a user added", attr: "ssh_access", rule: replaces,
					edit: set("ssh_access", fmt.Sprintf(`ssh_access = [{ username = "ops", ssh_public_keys = [%q] }]`, invKey))},
			),
			starts: []invStart{madeByApply, imported, failedWaits, failedKept, taintedWithToken, taintedNoToken},
		},
		{
			unexercised: map[string]string{"region": "the fake's projects are all in its one region, so a move is refused as a region its project isn't in, a row of its own"},
			typ:         instanceGroupType, noun: "instance group", guarded: true,
			replacing: []string{"cluster_type", "id", "node_pool", "project", "region", "ssh_access"},
			prelude:   noPrelude,
			base: func(name string) block {
				return block{
					"id":           q("id", name),
					"cluster_type": q("cluster_type", clusterTypeRef),
					"display_name": q("display_name", "inv"),
					"node_pool":    `node_pool = { "gpu-workers" = { size = 2 } }`,
				}
			},
			importID: scopedImport,
			pathOf: func(name string) string {
				return instanceGroupPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": name})
			},
			changes: append(scopeRows(`id = "acc-inv-renamed"`),
				invChange{label: "display_name", rule: changesInPlace, edit: set("display_name", q("display_name", "inv-renamed"))},
				// node_pool doc and the "What replaces it" paragraph: node_pool
				// is replaced when a pool shrinks, or when pools are added or
				// removed, and scaled in place when a pool grows.
				invChange{label: "node_pool, a larger size", rule: changesInPlace,
					edit: set("node_pool", `node_pool = { "gpu-workers" = { size = 3 } }`)},
				invChange{label: "node_pool, a smaller size", attr: "node_pool", rule: replaces,
					edit: set("node_pool", `node_pool = { "gpu-workers" = { size = 1 } }`)},
				invChange{label: "cluster_type, another ClusterType", attr: "cluster_type", rule: replaces,
					edit: func(b block) block {
						return b.with("cluster_type", q("cluster_type", "/v1/regions/us-texas-1/compute/cluster-types/cpu-general")).
							with("node_pool", `node_pool = { "workers" = { size = 1 } }`)
					}},
				invChange{label: "ssh_access, a user added", attr: "ssh_access", rule: replaces,
					edit: set("ssh_access", fmt.Sprintf(`ssh_access = [{ username = "ops", ssh_public_keys = [%q] }]`, invKey))},
			),
			starts: []invStart{madeByApply, imported, taintedWithToken},
		},
		{
			unexercised: map[string]string{"region": "the fake's projects are all in its one region, so a move is refused as a region its project isn't in, a row of its own"},
			typ:         fileStoreType, noun: "file store", guarded: true,
			replacing: []string{"id", "project", "region", "share_type"},
			prelude:   noPrelude,
			base: func(name string) block {
				return block{
					"id":           q("id", name),
					"display_name": q("display_name", "inv"),
					"share_type":   q("share_type", "general"),
					"capacity_gib": "capacity_gib = 100",
					"consumers":    `consumers = { "10.0.0.31" = { mount_path = "/mnt/datasets" } }`,
				}
			},
			importID: scopedImport,
			pathOf: func(name string) string {
				return fileStorePath(map[string]string{"region": "us-texas-1", "project": "demo", "id": name})
			},
			changes: append(scopeRows(`id = "acc-inv-renamed"`),
				invChange{label: "display_name", rule: changesInPlace, edit: set("display_name", q("display_name", "inv-renamed"))},
				// capacity_gib doc: raising it grows the store.
				invChange{label: "capacity_gib raised", rule: changesInPlace, edit: set("capacity_gib", "capacity_gib = 200")},
				// capacity_gib doc: lowering it is refused at plan time.
				invChange{label: "capacity_gib lowered", attr: "capacity_gib", rule: refused, refusal: shrink,
					edit: set("capacity_gib", "capacity_gib = 50")},
				invChange{label: "share_type", attr: "share_type", rule: replaces, edit: set("share_type", q("share_type", "high-speed"))},
			),
			starts: []invStart{madeByApply, imported},
		},
		{
			unexercised: map[string]string{"region": "the fake's projects are all in its one region, so a move is refused as a region its project isn't in, a row of its own",
				"infiniband": "every row starts from an evpn network, and moving to infiniband is refused naming evpn",
				"nvlink":     "every row starts from an evpn network"},
			typ: networkType, noun: "network", guarded: true,
			replacing: []string{"evpn", "id", "infiniband", "nvlink", "project", "region"},
			prelude:   noPrelude,
			base: func(name string) block {
				return block{
					"id":           q("id", name),
					"display_name": q("display_name", "inv"),
					"tags":         `tags = { team = "ml" }`,
					"evpn":         `evpn = { service_type = "l3vpn", vlan_id = "auto", mtu = 9000 }`,
				}
			},
			importID: scopedImport,
			pathOf: func(name string) string {
				return networkPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": name})
			},
			changes: append(scopeRows(`id = "acc-inv-renamed"`),
				invChange{label: "display_name", rule: changesInPlace, edit: set("display_name", q("display_name", "inv-renamed"))},
				invChange{label: "tags", rule: changesInPlace, edit: set("tags", `tags = { team = "infra" }`)},
				invChange{label: "evpn mtu", attr: "evpn", rule: replaces,
					edit: set("evpn", `evpn = { service_type = "l3vpn", vlan_id = "auto", mtu = 1500 }`)},
				invChange{label: "evpn to infiniband", attr: "evpn", rule: replaces,
					edit: func(b block) block {
						return b.with("evpn", "").with("infiniband", `infiniband = { partition = "training" }`)
					}},
			),
			starts: []invStart{madeByApply, imported},
		},
		{
			unexercised: map[string]string{"region": "the fake's projects are all in its one region, so a move is refused as a region its project isn't in, a row of its own"},
			typ:         subnetType, noun: "subnet", guarded: true,
			replacing: []string{"address_pool", "allocate_gateway", "description", "display_name", "exclude_ranges",
				"id", "network", "prefix_length", "project", "region"},
			prelude: func(name string) string {
				return fmt.Sprintf(`
resource %[1]q "test" {
  id   = "%[2]s-net"
  evpn = { service_type = "l3vpn", vlan_id = "auto" }
}
resource %[1]q "other" {
  id   = "%[2]s-net2"
  evpn = { service_type = "l3vpn", vlan_id = "auto" }
}
`, networkType, name)
			},
			base: func(name string) block {
				return block{
					"id":               q("id", name),
					"network":          fmt.Sprintf("network = %s.test.id", networkType),
					"address_pool":     q("address_pool", "/v1/regions/us-texas-1/networking/address-pools/tenant"),
					"prefix_length":    "prefix_length = 24",
					"allocate_gateway": "allocate_gateway = true",
					"exclude_ranges":   `exclude_ranges = ["10.0.0.10-10.0.0.20"]`,
				}
			},
			importID: func(name string) string { return "us-texas-1/demo/" + name + "-net/" + name },
			pathOf: func(name string) string {
				return subnetPath(map[string]string{"region": "us-texas-1", "project": "demo", "network": name + "-net", "id": name})
			},
			changes: append(scopeRows(`id = "acc-inv-renamed"`),
				invChange{label: "description", attr: "description", rule: replaces, edit: set("description", q("description", "renamed"))},
				invChange{label: "display_name", attr: "display_name", rule: replaces, edit: set("display_name", q("display_name", "renamed"))},
				invChange{label: "prefix_length", attr: "prefix_length", rule: replaces, edit: set("prefix_length", "prefix_length = 25")},
				invChange{label: "allocate_gateway", attr: "allocate_gateway", rule: replaces, edit: set("allocate_gateway", "allocate_gateway = false")},
				invChange{label: "exclude_ranges, other addresses", attr: "exclude_ranges", rule: replaces,
					edit: set("exclude_ranges", `exclude_ranges = ["10.0.0.10-10.0.0.30"]`)},
				// exclude_ranges doc: replaces only when the excluded addresses
				// change, so the same addresses written another way don't.
				invChange{label: "exclude_ranges, the same addresses split", rule: changesInPlace,
					edit: set("exclude_ranges", `exclude_ranges = ["10.0.0.10-10.0.0.15", "10.0.0.16-10.0.0.20"]`)},
				invChange{label: "network", attr: "network", rule: replaces, edit: set("network", fmt.Sprintf("network = %s.other.id", networkType))},
				invChange{label: "address_pool", attr: "address_pool", rule: replaces,
					edit: set("address_pool", q("address_pool", "/v1/regions/us-texas-1/networking/address-pools/cluster-local"))},
			),
			starts: []invStart{madeByApply, imported},
		},
		{
			unexercised: map[string]string{"region": "the fake has one region that can hold a project, so a move is refused, a row of its own"},
			typ:         projectType, noun: "project", guarded: true,
			replacing: []string{"id", "organization_id", "region"},
			prelude:   noPrelude,
			base: func(name string) block {
				return block{
					"id":              q("id", name),
					"organization_id": q("organization_id", accOrg),
					"display_name":    q("display_name", "Inv"),
					"description":     q("description", "inv"),
				}
			},
			importID: func(name string) string { return name },
			pathOf: func(name string) string {
				return projectPath(map[string]string{"organization_id": accOrg, "id": name})
			},
			changes: []invChange{
				{label: "nothing", rule: unchanged, edit: nothing},
				{label: "display_name", rule: changesInPlace, edit: set("display_name", q("display_name", "Renamed"))},
				{label: "description", rule: changesInPlace, edit: set("description", q("description", "renamed"))},
				{label: "id", attr: "id", rule: replaces, edit: set("id", `id = "acc-inv-renamed"`)},
				{label: "organization_id", attr: "organization_id", rule: replaces, edit: set("organization_id", q("organization_id", "org-other"))},
				// region doc: the region has to be one that can hold a project.
				// The fake has one, so a region move can only be this refusal.
				{label: "region", attr: "region", rule: refused, refusal: cantHoldProject,
					edit: set("region", `region = "us-elsewhere-1"`)},
			},
			starts: []invStart{madeByApply, imported},
		},
		{
			unexercised: map[string]string{"region": "the fake's projects are all in its one region, so a move is refused as a region its project isn't in, a row of its own"},
			typ:         securityGroupType, noun: "security group",
			replacing: []string{"id", "project", "region"},
			prelude:   noPrelude,
			base: func(name string) block {
				return block{
					"id":           q("id", name),
					"display_name": q("display_name", "inv"),
					"egress": `egress {
    action             = "permit"
    protocol           = "any"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
  }`,
				}
			},
			pathOf: func(name string) string {
				return securityGroupPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": name})
			},
			changes: append(scopeRows(`id = "acc-inv-renamed"`),
				invChange{label: "display_name", rule: changesInPlace, edit: set("display_name", q("display_name", "inv-renamed"))},
			),
			starts: []invStart{madeByApply},
		},
		{
			unexercised: map[string]string{"region": "the fake's projects are all in its one region, so a move is refused as a region its project isn't in, a row of its own"},
			typ:         peeringType, noun: "peering",
			replacing: []string{"description", "display_name", "id", "project", "region", "remote_vpc", "tags", "vpc"},
			prelude: func(name string) string {
				return clusterConfig(name+"-owner", 1)
			},
			base: func(name string) block {
				return block{
					"id":           q("id", name),
					"vpc":          q("vpc", "vpc-"+name+"-owner"),
					"remote_vpc":   q("remote_vpc", "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"),
					"display_name": q("display_name", "inv"),
					"depends_on":   fmt.Sprintf("depends_on = [%s.test]", clusterType),
				}
			},
			pathOf: func(name string) string {
				return peeringPath(map[string]string{"region": "us-texas-1", "project": "demo", "vpc": "vpc-" + name + "-owner", "id": name})
			},
			changes: append(scopeRows(`id = "acc-inv-renamed"`),
				invChange{label: "description", attr: "description", rule: replaces, edit: set("description", q("description", "renamed"))},
				invChange{label: "display_name", attr: "display_name", rule: replaces, edit: set("display_name", q("display_name", "renamed"))},
				invChange{label: "tags", attr: "tags", rule: replaces, edit: set("tags", `tags = { team = "ml" }`)},
				invChange{label: "vpc", attr: "vpc", rule: replaces, edit: set("vpc", q("vpc", "vpc-elsewhere"))},
				invChange{label: "remote_vpc, another VPC", attr: "remote_vpc", rule: replaces,
					edit: set("remote_vpc", q("remote_vpc", "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-training"))},
				// remote_vpc doc: moving between an id, a self_link, and a path
				// or absolute URL that name the same VPC replaces nothing, and
				// the plan is an in-place update.
				invChange{label: "remote_vpc, the same VPC as a URL", rule: changesInPlace,
					edit: func(b block) block {
						return b.with("remote_vpc", q("remote_vpc", endpoint+"/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"))
					}},
			),
			starts: []invStart{madeByApply},
		},
		{
			unexercised: map[string]string{"region": "the fake's projects are all in its one region, so a move is refused as a region its project isn't in, a row of its own"},
			typ:         vpcBindingType, noun: "binding",
			replacing:  []string{"project", "region", "vpc"},
			ownProject: true,
			prelude: func(name string) string {
				return clusterConfig(name+"-owner", 1) + fmt.Sprintf(`
resource %q "test" {
  id = "%s-sg"
  egress {
    action             = "permit"
    protocol           = "any"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
  }
}
`, securityGroupType, name)
			},
			base: func(name string) block {
				return block{
					"vpc":             q("vpc", "vpc-"+name+"-owner"),
					"security_groups": fmt.Sprintf("security_groups = [%s.test.id]", securityGroupType),
					"depends_on":      fmt.Sprintf("depends_on = [%s.test]", clusterType),
				}
			},
			pathOf: func(string) string { return "" },
			changes: []invChange{
				{label: "nothing", rule: unchanged, edit: nothing},
				{label: "security_groups", rule: changesInPlace, edit: set("security_groups", "security_groups = []")},
				{label: "vpc", attr: "vpc", rule: replaces, edit: set("vpc", q("vpc", "vpc-elsewhere"))},
				{label: "project", attr: "project", rule: replaces, edit: set("project", `project = "example"`)},
				{label: "region", attr: "region", rule: refused, refusal: regionNotProjects, onCreate: true,
					edit: set("region", `region = "us-elsewhere-1"`), silent: "refused"},
			},
			starts: []invStart{madeByApply},
		},
		{
			typ: webhookType, noun: "subscription",
			replacing: []string{"event_types", "url"},
			prelude:   noPrelude,
			base: func(string) block {
				return block{
					"url":               q("url", "https://hooks.example.com/inv"),
					"event_types":       `event_types = ["io.k0rdent.cluster.ready.v1"]`,
					"secret_wo":         `secret_wo = sha256("inv-subscription")`,
					"secret_wo_version": "secret_wo_version = 1",
				}
			},
			pathOf: func(string) string { return "" },
			changes: []invChange{
				{label: "nothing", rule: unchanged, edit: nothing},
				{label: "url", attr: "url", rule: replaces, edit: set("url", q("url", "https://hooks.example.com/other"))},
				{label: "event_types", attr: "event_types", rule: replaces,
					edit: set("event_types", `event_types = ["io.k0rdent.cluster.failed.v1"]`)},
			},
			starts: []invStart{madeByApply, turnedOff, turnedOffWithSecret},
			// secret_wo doc: required to create or replace a subscription.
			replaceRefused: map[invStart]*regexp.Regexp{turnedOff: regexp.MustCompile(`A new subscription needs a signing secret`)},
		},
	}
}
