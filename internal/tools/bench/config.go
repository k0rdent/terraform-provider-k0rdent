package main

import (
	"fmt"
	"strings"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The estate the benchmark generates, and why it's shaped this way.
//
// The dimension that matters is resource count in one configuration, so the
// generator takes a count and nothing else. What it fills that count with is a
// tenant's working set rather than one kind
// repeated: per ten resources, two networks, three subnets split across
// them, four security groups with rules and a file store. One block in ten
// trades a security group for a Kubernetes cluster,
// one trades a security group for an instance group, and one trades its file
// store for a webhook subscription. So a configuration of a thousand holds
// ten of each of those, which is the proportion of slow, chatty objects a real
// estate has, and every kind whose refresh reads more than its own object is
// in it: a cluster and an instance group look up the VPC they own, a subnet
// reads its address pool, and a webhook subscription has no read of one and
// lists them all.
//
// References between them are real ones (a subnet names its network by
// attribute), so Terraform's graph has the dependency edges a real
// configuration has and -parallelism has something to schedule around.

// kindCount is how many of one resource type a configuration of a given size
// holds, keyed by the type's suffix.
type kindCount map[string]int

// estate is one generated configuration.
type estate struct {
	size  int
	hcl   string
	kinds kindCount
}

// The cluster type every cluster and instance group is built from. The fake
// serves it, and its gpu-workers pool takes a count.
const benchClusterType = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"

// generate writes a configuration of exactly size managed resources. size has
// to be a positive multiple of ten, since the mix is per ten.
func generate(size int, pollInterval string) (estate, error) {
	if size <= 0 || size%10 != 0 {
		return estate{}, fmt.Errorf("size %d isn't a positive multiple of ten, and the mix is per ten resources", size)
	}
	var b strings.Builder
	kinds := kindCount{}
	add := func(suffix, name, body string) {
		kinds[suffix]++
		fmt.Fprintf(&b, "resource %q %q {\n%s}\n\n", provider.ResourceName(suffix), name, body)
	}

	fmt.Fprintf(&b, `terraform {
  required_providers {
    %s = {
      source = %q
    }
  }
}

variable "endpoint" {
  type = string
}

provider %q {
  endpoint          = var.endpoint
  region            = "us-texas-1"
  project           = "demo"
  operation_timeout = "10m"
  poll_interval     = %q
}

`, provider.ProviderTypeName, provider.RegistryNamespace+"/"+provider.ProviderTypeName,
		provider.ProviderTypeName, pollInterval)

	for block := range size / 10 {
		id := func(what string, n int) string { return fmt.Sprintf("bench-%s-%04d-%d", what, block, n) }

		for n := range 2 {
			add("network", id("net", n), fmt.Sprintf(`  id           = %q
  display_name = "benchmark network %d.%d"
  tags         = { team = "bench", block = "%d" }
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
    mtu          = 9000
  }
`, id("net", n), block, n, block))
		}
		for n := range 3 {
			add("subnet", id("subnet", n), fmt.Sprintf(`  id               = %q
  network          = %s.%s.id
  address_pool     = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length    = 24
  allocate_gateway = true
`, id("subnet", n), provider.ResourceName("network"), id("net", n%2)))
		}
		for n := range 4 {
			switch {
			case n == 0 && block%10 == 0:
				add("kubernetes_cluster", id("cluster", n), fmt.Sprintf(`  id           = %q
  cluster_type = %q
  display_name = "benchmark cluster %d"
  node_pool = {
    "gpu-workers" = { node_count = 2 }
  }
`, id("cluster", n), benchClusterType, block))
			case n == 0 && block%10 == 5:
				add("instance_group", id("group", n), fmt.Sprintf(`  id           = %q
  cluster_type = %q
  display_name = "benchmark hosts %d"
  node_pool = {
    "gpu-workers" = { size = 1 }
  }
`, id("group", n), benchClusterType, block))
			default:
				add("security_group", id("sg", n), fmt.Sprintf(`  id           = %q
  display_name = "benchmark group %d.%d"
  tags         = { team = "bench" }

  ingress {
    name                   = "ssh"
    action                 = "permit"
    protocol               = "tcp"
    source_prefix          = "203.0.113.0/24"
    destination_prefix     = "0.0.0.0/0"
    destination_port_range = "22"
  }
  ingress {
    name                   = "https"
    action                 = "permit"
    protocol               = "tcp"
    source_prefix          = "0.0.0.0/0"
    destination_prefix     = "0.0.0.0/0"
    destination_port_range = "443"
  }
  egress {
    action             = "permit"
    protocol           = "any"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
  }
`, id("sg", n), block, n))
			}
		}
		if block%10 == 9 {
			add("webhook_subscription", id("hook", 0), fmt.Sprintf(`  url               = "https://bench-hooks.example.test/%s"
  secret_wo         = sha256(%q)
  secret_wo_version = 1
`, id("hook", 0), id("hook", 0)))
		} else {
			add("file_store", id("fs", 0), fmt.Sprintf(`  id           = %q
  display_name = "benchmark store %d"
  share_type   = "general"
  capacity_gib = 100
  consumers = {
    "10.0.%d.31" = { mount_path = "/mnt/datasets" }
  }
`, id("fs", 0), block, block%250))
		}
	}
	return estate{size: size, hcl: b.String(), kinds: kinds}, nil
}

// total is every resource the estate holds.
func (k kindCount) total() int {
	n := 0
	for _, c := range k {
		n += c
	}
	return n
}
