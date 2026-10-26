package repocheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// A value a configuration must still hardcode, because the API does
// not let a tenant read it, is named in the documentation with an owner.
//
// # Why every input is classified
//
// "Hardcoded" is a judgement about what a value names, and no schema flag
// records it. So the check cannot find hardcoded values on its own. What it
// can do is refuse an input nobody has judged. Every argument a configuration
// sets, on every resource, data source, ephemeral resource, list resource,
// action and the provider block, is in exactly one of three places:
//
//   - free: the configuration's own data, such as a description, a count or a
//     CIDR, or the id of something the tenant made itself, which whoever made
//     it already has.
//   - read: it names a platform object, and a data source registered in this
//     provider reads it, so a configuration can take it from there. Where the
//     value sits on an attribute of that data source, the rule names the
//     attribute and the check asserts it exists.
//   - hardcodedValues: it names a platform object no tenant read supplies, so
//     it is on the published page with the side that has to change.
//
// A new argument fails the check until it is put in one of them, and putting
// it in the third means listing it on the page and linking the page from its
// description. The link runs one way only: an attribute that is not listed and
// still carries the known-gap note fails too, so an entry moved off the page
// cannot leave its note behind. A second net catches an attribute anywhere, input or not, whose
// name says it is a catalog value (a machine type, an image, a zone, a GPU),
// because those are exactly the ones a tenant can't read.
//
// Blocks are not walked: the provider has three kinds (timeouts and a security
// group's ingress and egress rules), all of them free.

// hardcodedValuesPage is the published guide that names each value.
const hardcodedValuesPage = "docs/guides/hardcoded-values.md"

// hardcodedValuesLink is what a description links to. It is relative to a
// resource or data source page, which sit one directory below docs/.
const hardcodedValuesLink = provider.HardcodedValuesLink

// owners are the sides a row may name. Never a person.
var owners = []string{"API", "Provider", "Product", "Platform"}

// attrRef names one attribute on one surface, by Terraform suffix so the
// provisional provider name is written nowhere here.
type attrRef struct {
	Kind   string // "resource", "data source", "ephemeral resource", "list resource", "action" or "provider"
	Suffix string // "" for the provider block, or for a rule covering every surface of Kind
	Path   string // dotted, as walkAttributes builds it
}

func (r attrRef) address() string {
	if r.Kind == "provider" {
		return provider.ProviderTypeName + "." + r.Path
	}
	return provider.ResourceName(r.Suffix) + "." + r.Path
}

func (r attrRef) String() string { return r.Kind + " " + r.address() }

// hardcodedValues is every attribute the page names. The inputs among them are
// the values a configuration must write as literals. The computed ones are
// where the unresolvable name surfaces, so a reader who finds it there is sent
// to the same page.
var hardcodedValues = []attrRef{
	// Machine type, GPU type and image: a tenant can't read a machine type, so
	// it cannot choose a ClusterType or a pool by hardware.
	{"resource", "kubernetes_cluster", "cluster_type"},
	{"resource", "kubernetes_cluster", "node_pool"},
	{"resource", "instance_group", "cluster_type"},
	{"resource", "instance_group", "node_pool"},
	{"data source", "cluster_type", "node_pools.machine_type"},
	{"data source", "cluster_type_version", "node_pools.machine_type"},
	{"data source", "instance_group", "node_pools.machine_type"},
	// Fabric partitions: operator-defined, and the contract has no read.
	{"resource", "network", "infiniband.partition"},
	{"resource", "network", "nvlink.partition"},
	// An organization with no project in it yet. An API key may get an empty
	// organization list, so the read gives the provider's credential nothing
	// to take the id from.
	{"resource", "project", "organization_id"},
	// The event types a subscription filters on. A tenant can't read the
	// catalog that lists them.
	{"resource", "webhook_subscription", "event_types"},
	// A folder, on the data sources that search a scope's service accounts.
	// The contract declares no folder operation at all, so no folder id can
	// be read.
	{"data source", "service_accounts", "scope_id"},
	{"data source", "service_account_credentials", "scope_id"},
}

// inputClass is how an input that is not on the page was judged.
type inputClass struct {
	// ReadBy is the data source suffix a configuration reads the value from.
	// Empty means the value is free: the configuration's own data.
	ReadBy string
	// From is the attribute on ReadBy that carries the value, where naming it
	// is the point: a value that used to be on the page names where it is
	// read now. Empty when the data source as a whole is the answer.
	From string
}

var (
	free   = inputClass{}
	read   = func(suffix string) inputClass { return inputClass{ReadBy: suffix} }
	readAt = func(suffix, from string) inputClass { return inputClass{ReadBy: suffix, From: from} }
)

// inputClasses judges every input not in hardcodedValues. A rule with an empty
// Suffix covers that path on every surface of its Kind, and a rule naming a
// surface overrides it.
var inputClasses = map[attrRef]inputClass{
	// The provider block.
	{"provider", "", "default_tags"}:      free,
	{"provider", "", "endpoint"}:          free,
	{"provider", "", "operation_timeout"}: free,
	{"provider", "", "poll_interval"}:     free,
	{"provider", "", "project"}:           read("projects"),
	{"provider", "", "region"}:            read("regions"),

	// Every surface that takes a scope override.
	{"resource", "", "project"}:           read("projects"),
	{"resource", "", "region"}:            read("regions"),
	{"data source", "", "project"}:        read("projects"),
	{"data source", "", "region"}:         read("regions"),
	{"ephemeral resource", "", "project"}: read("projects"),
	{"ephemeral resource", "", "region"}:  read("regions"),

	// A managed resource's own name and labels are the configuration's to choose.
	{"resource", "", "id"}:           free,
	{"resource", "", "display_name"}: free,
	{"resource", "", "description"}:  free,
	{"resource", "", "tags"}:         free,
	// The configuration's own permission to replace a resource holding tenant state.
	{"resource", "", "allow_replace"}: free,
	// The configuration's own choice of what a destroy does with a cluster mid-operation.
	{"resource", "kubernetes_cluster", "on_destroy_busy"}: free,

	// Resources. An id of something the tenant made itself is free: the
	// configuration that made it, or the person who did, already has it.
	{"resource", "file_store", "capacity_gib"}:           free,
	{"resource", "file_store", "share_type"}:             read("share_types"),
	{"resource", "file_store", "consumers"}:              free,
	{"resource", "file_store", "consumers.type"}:         free,
	{"resource", "file_store", "consumers.access_level"}: free,
	{"resource", "file_store", "consumers.mount_path"}:   free,
	{"resource", "file_store", "consumers.network"}:      free,
	{"resource", "file_store", "consumers.network.type"}: free,
	// Listed on the page until the instances data source carried them.
	{"resource", "file_store", "consumers.network.mac_address"}: readAt("instances", "instances.network_interfaces.mac_address"),
	{"resource", "file_store", "consumers.network.guid"}:        readAt("instances", "instances.network_interfaces.guid"),
	{"resource", "instance_group", "node_pool.size"}:            free,
	{"resource", "kubernetes_cluster", "node_pool.node_count"}:  free,
	{"resource", "kubernetes_cluster", "security_groups"}:       read("security_groups"),
	{"resource", "kubernetes_cluster", "storage_ceiling_gib"}:   free,
	// The regions that hold a project, which the contract asks a client to
	// offer rather than every regional one.
	{"resource", "project", "region"}:                         readAt("regions", "project_region_ids"),
	{"resource", "network", "evpn"}:                           free,
	{"resource", "network", "evpn.service_type"}:              free,
	{"resource", "network", "evpn.vlan_id"}:                   free,
	{"resource", "network", "evpn.dhcp_enabled"}:              free,
	{"resource", "network", "evpn.dns_servers"}:               free,
	{"resource", "network", "evpn.mtu"}:                       free,
	{"resource", "network", "infiniband"}:                     free,
	{"resource", "network", "infiniband.pkey"}:                free,
	{"resource", "network", "infiniband.index0"}:              free,
	{"resource", "network", "infiniband.ip_over_ib"}:          free,
	{"resource", "network", "nvlink"}:                         free,
	{"resource", "subnet", "network"}:                         read("networks"),
	{"resource", "subnet", "address_pool"}:                    read("address_pools"),
	{"resource", "subnet", "prefix_length"}:                   free,
	{"resource", "subnet", "allocate_gateway"}:                free,
	{"resource", "subnet", "exclude_ranges"}:                  free,
	{"resource", "vpc_peering", "vpc"}:                        read("vpcs"),
	{"resource", "vpc_peering", "remote_vpc"}:                 read("vpc"),
	{"resource", "vpc_security_groups", "vpc"}:                read("vpcs"),
	{"resource", "vpc_security_groups", "security_groups"}:    read("security_groups"),
	{"resource", "webhook_subscription", "url"}:               free,
	{"resource", "webhook_subscription", "secret_wo"}:         free,
	{"resource", "webhook_subscription", "secret_wo_version"}: free,
	{"resource", "webhook_subscription", "rotation_overlap"}:  free,

	// The users and public keys a group's hosts, or a cluster's nodes, let in
	// are the tenant's own.
	{"resource", "instance_group", "ssh_access"}:                     free,
	{"resource", "instance_group", "ssh_access.username"}:            free,
	{"resource", "instance_group", "ssh_access.ssh_public_keys"}:     free,
	{"resource", "instance_group", "ssh_access.sudo"}:                free,
	{"resource", "instance_group", "ssh_access.display_name"}:        free,
	{"resource", "kubernetes_cluster", "ssh_access"}:                 free,
	{"resource", "kubernetes_cluster", "ssh_access.username"}:        free,
	{"resource", "kubernetes_cluster", "ssh_access.ssh_public_keys"}: free,
	{"resource", "kubernetes_cluster", "ssh_access.sudo"}:            free,
	{"resource", "kubernetes_cluster", "ssh_access.display_name"}:    free,

	// A service account sits in an organization, which the projects data
	// source reports per project. The level searched is the configuration's
	// own, and an account is one the accounts data source lists.
	{"data source", "roles", "organization_id"}:                          read("projects"),
	{"data source", "roles", "scope"}:                                    free,
	{"data source", "service_account", "id"}:                             readAt("service_accounts", "service_accounts.id"),
	{"data source", "service_account", "organization_id"}:                read("projects"),
	{"data source", "service_accounts", "organization_id"}:               read("projects"),
	{"data source", "service_accounts", "scope"}:                         free,
	{"data source", "service_account_credentials", "organization_id"}:    read("projects"),
	{"data source", "service_account_credentials", "scope"}:              free,
	{"data source", "service_account_credentials", "service_account_id"}: readAt("service_accounts", "service_accounts.id"),
	{"data source", "service_account_credentials", "status"}:             free,
	{"data source", "service_accounts", "status"}:                        free,

	{"ephemeral resource", "cluster_kubeconfig", "cluster_id"}:  free,
	{"data source", "cluster_kubeconfig", "cluster_id"}:         free,
	{"data source", "cluster_kubeconfig", "id"}:                 free,
	{"data source", "kubernetes_cluster", "id"}:                 free,
	{"data source", "instance_group", "id"}:                     free,
	{"data source", "file_store", "id"}:                         free,
	{"data source", "effective_security_groups", "object_kind"}: free,
	{"data source", "effective_security_groups", "object_id"}:   free,
	{"data source", "address_pool", "id"}:                       read("address_pools"),
	{"data source", "address_pools", "only_routable"}:           free,
	{"data source", "cluster_type", "id"}:                       read("cluster_types"),
	{"data source", "cluster_types", "include_deprecated"}:      free,
	{"data source", "cluster_type_version", "cluster_type"}:     read("cluster_types"),
	{"data source", "cluster_type_version", "version_id"}:       read("cluster_type_versions"),
	{"data source", "cluster_type_versions", "cluster_type"}:    read("cluster_types"),
	{"data source", "cluster_type_versions", "status"}:          free,
	{"data source", "network", "id"}:                            read("networks"),
	{"data source", "networks", "only_tenant_owned"}:            free,
	{"data source", "subnet", "id"}:                             read("subnets"),
	{"data source", "subnet", "network"}:                        read("networks"),
	{"data source", "subnets", "network"}:                       read("networks"),
	{"data source", "security_group", "id"}:                     read("security_groups"),
	{"data source", "security_groups", "only_tenant_owned"}:     free,
	{"data source", "share_type", "id"}:                         read("share_types"),
	{"data source", "vpc", "id"}:                                read("vpcs"),
	{"data source", "vpc_peering", "id"}:                        read("vpc_peerings"),
	{"data source", "vpc_peering", "vpc"}:                       read("vpcs"),
	{"data source", "vpc_peerings", "vpc"}:                      read("vpcs"),
	{"data source", "regions", "include_global"}:                free,
	// The plural data sources' filters narrow a list by what its objects
	// carry. The values are the configuration's own choice of what to find.
	{"data source", "", "tags"}:                              free,
	{"data source", "", "display_name_regex"}:                free,
	{"data source", "", "state"}:                             free,
	{"data source", "cluster_types", "status"}:               free,
	{"data source", "projects", "status"}:                    free,
	{"data source", "webhook_subscriptions", "is_active"}:    free,
	{"data source", "project", "id"}:                         read("projects"),
	{"data source", "project", "organization_id"}:            read("projects"),
	{"data source", "projects", "organization_id"}:           read("projects"),
	{"data source", "project_quota", "organization_id"}:      read("projects"),
	{"data source", "project_quota", "project_id"}:           read("projects"),
	{"data source", "tenant_quota", "organization_id"}:       read("projects"),
	{"data source", "organization_quota", "organization_id"}: read("projects"),
	{"data source", "organization", "id"}:                    readAt("organizations", "ids"),
	{"data source", "organizations", "status"}:               free,
	{"data source", "instance", "id"}:                        readAt("instances", "ids"),
	{"data source", "instances", "instance_group"}:           free,
	{"data source", "instances", "node_pool"}:                readAt("instance_group", "node_pools"),
	{"data source", "webhook_subscription", "id"}:            readAt("webhook_subscriptions", "subscriptions.id"),
	{"data source", "webhook_subscriptions", "url"}:          free,

	// A list block's configuration says where to search, and which of what
	// it finds to show.
	{"list resource", "", "project"}:                read("projects"),
	{"list resource", "", "region"}:                 read("regions"),
	{"list resource", "", "include_platform_owned"}: free,
	{"list resource", "project", "organization_id"}: read("projects"),
	{"list resource", "subnet", "network"}:          read("networks"),
	{"list resource", "vpc_peering", "vpc"}:         read("vpcs"),

	// The cancel action's config block names a cluster the tenant made, where
	// it is, and optionally the operation the cluster data source reports.
	{"action", "cluster_cancel_operation", "cluster_id"}:   free,
	{"action", "cluster_cancel_operation", "region"}:       read("regions"),
	{"action", "cluster_cancel_operation", "project"}:      read("projects"),
	{"action", "cluster_cancel_operation", "execution_id"}: readAt("kubernetes_cluster", "operation.execution_id"),
}

// classify returns how an input was judged, and whether it was.
func classify(kind, suffix, path string) (inputClass, bool) {
	if c, ok := inputClasses[attrRef{kind, suffix, path}]; ok {
		return c, true
	}
	c, ok := inputClasses[attrRef{kind, "", path}]
	return c, ok
}

// catalogName is the second net: an attribute named like a catalog value.
var catalogName = regexp.MustCompile(`(^|[._])(machine_types?|images?|zones?|gpu[a-z_]*)$`)

// attributeToken finds an attribute address written in backticks on the page.
var attributeToken = regexp.MustCompile("`(" + regexp.QuoteMeta(provider.ProviderTypeName) + `_[a-z_]+\.[a-z_.]+)` + "`")

// suffixOf turns a registered type name back into its suffix.
func suffixOf(s surface) string {
	if s.Kind == "provider" {
		return ""
	}
	return strings.TrimPrefix(s.Name, provider.ProviderTypeName+"_")
}

func isHardcoded(kind, suffix, path string) bool {
	for _, h := range hardcodedValues {
		if h.Kind == kind && h.Suffix == suffix && h.Path == path {
			return true
		}
	}
	return false
}

// everyHardcodedValueIsNamed reads the published page under root. See the
// comment at the top of this file for what it can and cannot see.
func everyHardcodedValueIsNamed(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, hardcodedValuesPage)) //nolint:gosec // a path inside this repository
	if err != nil {
		return fmt.Errorf("read %s: %w", hardcodedValuesPage, err)
	}
	page := string(raw)
	problems, _, used := judgeInputs()
	problems = append(problems, staleRules(used)...)
	problems = append(problems, checkHardcodedValues(page)...)
	problems = append(problems, checkPageRows(page)...)
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// judgeInputs walks every surface and reports inputs nobody judged, catalog
// names not on the page, and read rules naming a data source that does not
// exist. It also returns the rules it used, so an unused one can be reported.
func judgeInputs() (problems []string, inputs int, used map[attrRef]bool) {
	used = map[attrRef]bool{}
	all := surfaces()
	dataSources := map[string]surface{}
	for _, s := range all {
		if s.Kind == "data source" {
			dataSources[suffixOf(s)] = s
		}
	}
	for _, s := range all {
		suffix := suffixOf(s)
		walkAttributes(s.Attributes, "", func(path string, a attribute) {
			ref := attrRef{s.Kind, suffix, path}
			hardcoded := isHardcoded(s.Kind, suffix, path)
			if catalogName.MatchString(path) && !hardcoded {
				problems = append(problems, fmt.Sprintf("%s is named like a catalog value and is not on %s", ref, hardcodedValuesPage))
			}
			if strings.Contains(a.Doc, hardcodedValuesLink) && !hardcoded {
				problems = append(problems, fmt.Sprintf("%s links %s from its description and is not on the page: drop the known-gap note or list it", ref, hardcodedValuesLink))
			}
			if !a.Required && !a.Optional {
				return
			}
			inputs++
			if hardcoded {
				return
			}
			c, ok := classify(s.Kind, suffix, path)
			if !ok {
				problems = append(problems, fmt.Sprintf("%s is an input nobody has judged: classify it in internal/repocheck/hardcoded_values_test.go as free, read through a data source, or hardcoded and named on %s", ref, hardcodedValuesPage))
				return
			}
			if _, exact := inputClasses[ref]; exact {
				used[ref] = true
			} else {
				used[attrRef{s.Kind, "", path}] = true
			}
			if c.ReadBy == "" {
				return
			}
			ds, registered := dataSources[c.ReadBy]
			if !registered {
				problems = append(problems, fmt.Sprintf("%s is said to be read through %s, which is not a registered data source", ref, provider.ResourceName(c.ReadBy)))
				return
			}
			if c.From != "" {
				if _, ok := ds.lookup(c.From); !ok {
					problems = append(problems, fmt.Sprintf("%s is said to be read from %s.%s, which has no such attribute", ref, provider.ResourceName(c.ReadBy), c.From))
				}
			}
		})
	}
	return problems, inputs, used
}

func staleRules(used map[attrRef]bool) []string {
	var problems []string
	for ref := range inputClasses {
		if !used[ref] {
			problems = append(problems, fmt.Sprintf("the rule for %s %s matches no input any more: remove it", ref.Kind, ref.Path))
		}
	}
	return problems
}

// checkHardcodedValues asserts each listed attribute exists, is on the page,
// and links the page from its own description.
func checkHardcodedValues(page string) []string {
	var problems []string
	for _, h := range hardcodedValues {
		s, ok := findSurface(h.Kind, provider.ResourceName(h.Suffix))
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: no such %s is registered", h, h.Kind))
			continue
		}
		a, ok := s.lookup(h.Path)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: no such attribute", h))
			continue
		}
		if !strings.Contains(page, "`"+h.address()+"`") {
			problems = append(problems, fmt.Sprintf("%s is not named on %s", h, hardcodedValuesPage))
		}
		if !strings.Contains(a.Doc, hardcodedValuesLink) {
			problems = append(problems, fmt.Sprintf("%s: its description does not link %s", h, hardcodedValuesLink))
		}
	}
	return problems
}

// checkPageRows asserts every table row naming an attribute names one the list
// above holds, and gives an owner from the four sides.
func checkPageRows(page string) []string {
	var problems []string
	for _, line := range strings.Split(page, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		tokens := attributeToken.FindAllStringSubmatch(line, -1)
		if len(tokens) == 0 {
			continue
		}
		for _, t := range tokens {
			if !pageNames(t[1]) {
				problems = append(problems, fmt.Sprintf("%s names %s, which hardcodedValues does not hold", hardcodedValuesPage, t[1]))
			}
		}
		if !rowHasOwner(line) {
			problems = append(problems, fmt.Sprintf("%s: the row for %s names no owner from %s", hardcodedValuesPage, tokens[0][1], strings.Join(owners, ", ")))
		}
	}
	return problems
}

func pageNames(address string) bool {
	for _, h := range hardcodedValues {
		if h.address() == address {
			return true
		}
	}
	return false
}

func rowHasOwner(line string) bool {
	for _, cell := range strings.Split(line, "|") {
		for _, o := range owners {
			if strings.TrimSpace(cell) == o {
				return true
			}
		}
	}
	return false
}

// runAgainstPage runs the check with the published page replaced by page.
func runAgainstPage(t *testing.T, page string) error {
	t.Helper()
	root := t.TempDir()
	dst := filepath.Join(root, hardcodedValuesPage)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	return everyHardcodedValueIsNamed(root)
}

func publishedPage(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), hardcodedValuesPage))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestHardcodedValuesPagePassesAsPublished(t *testing.T) {
	if err := runAgainstPage(t, publishedPage(t)); err != nil {
		t.Fatalf("the published page should pass: %v", err)
	}
}

func TestHardcodedValuesPageMissingARowFails(t *testing.T) {
	address := "`" + provider.ResourceName("network") + ".nvlink.partition`"
	var kept []string
	for _, line := range strings.Split(publishedPage(t), "\n") {
		if !strings.Contains(line, address) {
			kept = append(kept, line)
		}
	}
	err := runAgainstPage(t, strings.Join(kept, "\n"))
	if err == nil || !strings.Contains(err.Error(), "nvlink.partition is not named") {
		t.Fatalf("dropping a row should fail the check, got %v", err)
	}
}

func TestHardcodedValuesRowWithoutOwnerFails(t *testing.T) {
	address := "`" + provider.ResourceName("project") + ".organization_id`"
	var out []string
	for _, line := range strings.Split(publishedPage(t), "\n") {
		if strings.Contains(line, address) {
			line = strings.Replace(line, "| API |", "| somebody |", 1)
		}
		out = append(out, line)
	}
	err := runAgainstPage(t, strings.Join(out, "\n"))
	if err == nil || !strings.Contains(err.Error(), "names no owner") {
		t.Fatalf("a row with no owner should fail the check, got %v", err)
	}
}

func TestHardcodedValuesPageNamingAnUnlistedAttributeFails(t *testing.T) {
	extra := "| `" + provider.ResourceName("subnet") + ".address_pool` | resource | x | 200 | API | none |\n"
	err := runAgainstPage(t, publishedPage(t)+"\n"+extra)
	if err == nil || !strings.Contains(err.Error(), "which hardcodedValues does not hold") {
		t.Fatalf("a page row the list does not hold should fail the check, got %v", err)
	}
}

// A new argument nobody has judged is the case the check exists for. Removing
// a judgement stands in for adding an argument: either way an input has none.
func TestAnUnjudgedInputFails(t *testing.T) {
	ref := attrRef{"resource", "subnet", "address_pool"}
	saved := inputClasses[ref]
	delete(inputClasses, ref)
	t.Cleanup(func() { inputClasses[ref] = saved })

	err := runAgainstPage(t, publishedPage(t))
	if err == nil || !strings.Contains(err.Error(), "address_pool is an input nobody has judged") {
		t.Fatalf("an unjudged input should fail the check, got %v", err)
	}
}

func TestAReadThroughAMissingDataSourceFails(t *testing.T) {
	ref := attrRef{"resource", "subnet", "address_pool"}
	saved := inputClasses[ref]
	inputClasses[ref] = read("address_pool_catalogue")
	t.Cleanup(func() { inputClasses[ref] = saved })

	err := runAgainstPage(t, publishedPage(t))
	if err == nil || !strings.Contains(err.Error(), "not a registered data source") {
		t.Fatalf("a read through a missing data source should fail the check, got %v", err)
	}
}

// A read rule that names the attribute a value is read from has to name one
// the data source has, or the guide's "read through a data source" is a claim
// nothing holds.
func TestAReadFromAMissingAttributeFails(t *testing.T) {
	ref := attrRef{"resource", "file_store", "consumers.network.mac_address"}
	saved := inputClasses[ref]
	inputClasses[ref] = readAt("instances", "instances.network_interfaces.mac")
	t.Cleanup(func() { inputClasses[ref] = saved })

	err := runAgainstPage(t, publishedPage(t))
	if err == nil || !strings.Contains(err.Error(), "network_interfaces.mac, which has no such attribute") {
		t.Fatalf("a read from an attribute the data source lacks should fail the check, got %v", err)
	}
}

// Taking an attribute off the list without dropping its note leaves a
// description that sends the reader to a page that no longer names it.
func TestAKnownGapNoteOffThePageFails(t *testing.T) {
	saved := hardcodedValues
	ref := attrRef{"resource", "project", "organization_id"}
	hardcodedValues = nil
	for _, h := range saved {
		if h != ref {
			hardcodedValues = append(hardcodedValues, h)
		}
	}
	inputClasses[ref] = readAt("organizations", "ids")
	t.Cleanup(func() {
		hardcodedValues = saved
		delete(inputClasses, ref)
	})

	var kept []string
	for _, line := range strings.Split(publishedPage(t), "\n") {
		if !strings.Contains(line, "`"+ref.address()+"`") || !strings.HasPrefix(line, "|") {
			kept = append(kept, line)
		}
	}
	err := runAgainstPage(t, strings.Join(kept, "\n"))
	if err == nil || !strings.Contains(err.Error(), "organization_id links "+hardcodedValuesLink) {
		t.Fatalf("a known-gap note off the page should fail the check, got %v", err)
	}
}

func TestACatalogNamedAttributeOffThePageFails(t *testing.T) {
	saved := hardcodedValues
	hardcodedValues = nil
	for _, h := range saved {
		if h.Path != "node_pools.machine_type" || h.Suffix != "cluster_type" {
			hardcodedValues = append(hardcodedValues, h)
		}
	}
	t.Cleanup(func() { hardcodedValues = saved })

	err := runAgainstPage(t, publishedPage(t))
	if err == nil || !strings.Contains(err.Error(), "is named like a catalog value") {
		t.Fatalf("a machine type attribute off the list should fail the check, got %v", err)
	}
}
