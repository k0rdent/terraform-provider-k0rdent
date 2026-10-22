package provider

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"regexp/syntax"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"go.yaml.in/yaml/v3"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// Plan-time validation, generated from the contract.
//
// Every constraint the contract puts on a value the provider sends (an enum,
// a pattern, a length, a range, a count, a format) is a 422 at apply time
// unless the provider refuses the same value at plan time. The list of those
// constraints comes from the spec this build resolves, read here, not from
// anybody's reading of it: every operation the provider calls, each of its
// path and query parameters, and its request body at every depth.
//
// Each constraint then needs an entry in specBindings, keyed by operation and
// location. An entry either names the attribute that carries the value, and
// then that attribute's own validators are probed with values the contract
// refuses and values it accepts, or it says why no configuration can reach
// the value. So a constraint the contract adds tomorrow fails
// TestEverySpecConstraintIsCheckedAtPlanTime until it is bound or explained,
// and a binding whose validator doesn't hold the line fails it too.

// specConstraint is one constrained value: where it is, and the keywords that
// constrain it.
type specConstraint struct {
	op, loc string
	schema  map[string]any
}

func (c specConstraint) key() string { return c.op + " " + c.loc }

// constraintKeywords are the JSON Schema keywords that limit a value.
var constraintKeywords = []string{
	"enum", "const", "pattern", "minLength", "maxLength", "minimum", "maximum",
	"exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minItems", "maxItems",
	"uniqueItems", "minProperties", "maxProperties", "format",
}

// probeMode says what part of an attribute a constraint lands on.
type probeMode int

const (
	// asValue: the attribute's own value, a string or a number.
	asValue probeMode = iota
	// asElement: each element of a list or set of strings.
	asElement
	// asKey: each key of a map.
	asKey
	// asSize: how many elements a list, set or map holds.
	asSize
)

// specBinding is where a constraint is checked, or why it can't be.
type specBinding struct {
	kind, suffix, attr string
	mode               probeMode
	// wrap turns a probe into the attribute's value, for a constraint on one
	// part of a reference, such as the version in a ClusterType self-link.
	wrap func(string) string
	// reason says why no configuration can reach the value. Set instead of
	// the fields above.
	reason string
	// stricter says why the attribute refuses more than the contract does,
	// such as a CIDR check on a field the contract types as a short string.
	// The values the contract accepts are then not all probed, only accepts,
	// which both take, so the check still shows it lets real values through.
	stricter string
	accepts  []string
}

func onResource(suffix, attr string, mode probeMode) specBinding {
	return specBinding{kind: "resource", suffix: suffix, attr: attr, mode: mode}
}

func onDataSource(suffix, attr string, mode probeMode) specBinding {
	return specBinding{kind: "data source", suffix: suffix, attr: attr, mode: mode}
}

func notConfigurable(reason string) specBinding { return specBinding{reason: reason} }

const (
	paging        = "a paging parameter the provider sets itself while it walks a list"
	notSent       = "the provider doesn't send this parameter"
	uriReference  = "`uri-reference` holds any reference, so there is nothing to refuse at plan time. The provider parses the reference itself before any part of it reaches a URL"
	discriminator = "the variant's discriminator, which the provider sets from the nested block the configuration fills in"
	nonEmptyPatch = "the provider sends an update only with a field in it"
)

// specBindings binds every constraint the test finds. The keys are the
// operation and the location in it, as the failure message prints them.
var specBindings = func() map[string]specBinding {
	b := map[string]specBinding{}
	each := func(ops []string, loc string, binding specBinding) {
		for _, op := range ops {
			b[op+" "+loc] = binding
		}
	}

	// Clusters.
	cluster := clusterResourceSuffix
	b["createCluster body.id"] = onResource(cluster, "id", asValue)
	b["createCluster body.clusterType"] = notConfigurable(uriReference)
	b["createCluster body.auditPolicy"] = notConfigurable("the cluster resource has no audit policy attribute, so the provider never sends one")
	for _, loc := range []string{"body.sshAccess", "body.sshAccess[].username", "body.sshAccess[].sshPublicKeys",
		"body.sshAccess[].sshPublicKeys[]", "body.sshAccess[].sudo", "body.sshAccess[].displayName"} {
		b["createCluster "+loc] = notConfigurable("the cluster resource has no ssh_access attribute, so the provider never sends one")
	}
	each([]string{"createCluster", "updateCluster", "scaleCluster"}, "body.nodePools[].id", onResource(cluster, "node_pool", asKey))
	each([]string{"createCluster", "updateCluster", "scaleCluster"}, "body.nodePools[].nodeCount", onResource(cluster, "node_pool.node_count", asValue))
	b["scaleCluster body.nodePools"] = notConfigurable("the provider scales only when a pool it manages changes, so the list it sends always has one")
	each([]string{"createCluster", "updateCluster"}, "body.storage.ceilingGiB", onResource(cluster, "storage_ceiling_gib", asValue))
	each([]string{"createCluster", "updateCluster"}, "body.securityGroups", onResource(cluster, "security_groups", asSize))
	each([]string{"createCluster", "updateCluster"}, "body.securityGroups[]", onResource(cluster, "security_groups", asElement))
	b["cancelCluster body.executionId"] = notConfigurable("the provider sends the execution id the cluster's " +
		"own read names, and refuses to cancel when the cluster names none")
	b["upgradeCluster body.clusterTypeVersionId"] = specBinding{kind: "resource", suffix: cluster, attr: "cluster_type", mode: asValue,
		wrap: func(v string) string { return "/v1/regions/r/compute/cluster-types/t" + versionsSegment + v }}

	// Instance groups.
	group := instanceGroupResourceSuffix
	b["createInstanceGroup body.id"] = onResource(group, "id", asValue)
	b["createInstanceGroup body.clusterType"] = notConfigurable(uriReference)
	b["createInstanceGroup body.nodePools"] = onResource(group, "node_pool", asSize)
	b["scaleInstanceGroup body.nodePools"] = notConfigurable("the provider scales only when a pool changes, so the list it sends always has one")
	each([]string{"createInstanceGroup", "scaleInstanceGroup"}, "body.nodePools[].nodePool", onResource(group, "node_pool", asKey))
	each([]string{"createInstanceGroup", "scaleInstanceGroup"}, "body.nodePools[].size", onResource(group, "node_pool.size", asValue))
	b["createInstanceGroup body.sshAccess"] = onResource(group, "ssh_access", asSize)
	b["createInstanceGroup body.sshAccess[].username"] = onResource(group, "ssh_access.username", asValue)
	b["createInstanceGroup body.sshAccess[].sshPublicKeys"] = onResource(group, "ssh_access.ssh_public_keys", asSize)
	b["createInstanceGroup body.sshAccess[].sshPublicKeys[]"] = onResource(group, "ssh_access.ssh_public_keys", asElement)
	b["createInstanceGroup body.sshAccess[].sudo"] = onResource(group, "ssh_access.sudo", asValue)
	b["createInstanceGroup body.sshAccess[].displayName"] = onResource(group, "ssh_access.display_name", asValue)
	for _, op := range []string{"createInstanceGroup", "updateInstanceGroup"} {
		for _, loc := range []string{"body.securityGroups", "body.securityGroups[]"} {
			b[op+" "+loc] = notConfigurable("the instance group resource has no security_groups attribute, so the provider never sends one")
		}
	}

	// File stores.
	store := fileStoreResourceSuffix
	b["createFileStore body.id"] = onResource(store, "id", asValue)
	each([]string{"createFileStore", "updateFileStore"}, "body.shareType", onResource(store, "share_type", asValue))
	each([]string{"createFileStore", "updateFileStore"}, "body.capacityGiB", onResource(store, "capacity_gib", asValue))
	each([]string{"createFileStore", "updateFileStore"}, "body.consumers", onResource(store, "consumers", asSize))
	each([]string{"createFileStore", "updateFileStore"}, "body.consumers[].type", onResource(store, "consumers.type", asValue))
	each([]string{"createFileStore", "updateFileStore"}, "body.consumers[].network.type", onResource(store, "consumers.network.type", asValue))
	each([]string{"createFileStore", "updateFileStore"}, "body.consumers[].network.macAddress", onResource(store, "consumers.network.mac_address", asValue))
	each([]string{"createFileStore", "updateFileStore"}, "body.consumers[].network.guid", onResource(store, "consumers.network.guid", asValue))
	each([]string{"createFileStore", "updateFileStore"}, "body.consumers[].accessLevel", onResource(store, "consumers.access_level", asValue))
	each([]string{"createFileStore", "updateFileStore"}, "body.consumers[].mountPath", onResource(store, "consumers.mount_path", asValue))
	b["updateFileStore body"] = notConfigurable(nonEmptyPatch)

	// Networks: the request is one of three variants, one per fabric.
	network := networkResourceSuffix
	for i := range 3 { // evpn, infiniband, nvlink
		at := fmt.Sprintf("body|oneOf%d.", i)
		b["createNetwork "+at+"id"] = onResource(network, "id", asValue)
		b["createNetwork "+at+"type"] = notConfigurable(discriminator)
	}
	b["createNetwork body|oneOf0.evpn.type"] = onResource(network, "evpn.service_type", asValue)
	b["createNetwork body|oneOf0.vlan.id"] = onResource(network, "evpn.vlan_id", asValue)
	b["createNetwork body|oneOf0.mtu"] = onResource(network, "evpn.mtu", asValue)
	b["createNetwork body|oneOf1.infiniband.pkey"] = onResource(network, "infiniband.pkey", asValue)
	b["createNetwork body|oneOf1.infiniband.partition"] = onResource(network, "infiniband.partition", asValue)
	b["createNetwork body|oneOf2.nvlink.partition"] = onResource(network, "nvlink.partition", asValue)

	// Subnets, peerings, security groups, projects.
	b["createSubnet body.id"] = onResource(subnetResourceSuffix, "id", asValue)
	b["createSubnet body.addressPool"] = notConfigurable(uriReference)
	b["createSubnet body.prefixLength"] = onResource(subnetResourceSuffix, "prefix_length", asValue)
	b["createVPCPeering body.id"] = onResource(vpcPeeringResourceSuffix, "id", asValue)
	b["createVPCPeering body.remoteVpc"] = notConfigurable(uriReference)
	b["setVPCSecurityGroups body.securityGroups"] = onResource(vpcSecurityGroupsResourceSuffix, "security_groups", asSize)
	b["setVPCSecurityGroups body.securityGroups[]"] = onResource(vpcSecurityGroupsResourceSuffix, "security_groups", asElement)
	b["createProjectInOrg body.id"] = onResource(projectResourceSuffix, "id", asValue)
	b["createProjectInOrg body.region"] = onResource(projectResourceSuffix, "region", asValue)
	b["updateProjectInOrg body"] = notConfigurable(nonEmptyPatch)

	// Webhook subscriptions. The id in a path is one the platform assigned
	// and the provider read back, or one an import held to a UUID before it
	// reached state.
	webhook := webhookSubscriptionResourceSuffix
	each([]string{"createWebhookSubscription", "rotateWebhookSubscriptionSecret"}, "body.secret", onResource(webhook, "secret_wo", asValue))
	each([]string{"deleteWebhookSubscription", "rotateWebhookSubscriptionSecret"}, "path:id",
		notConfigurable("the platform assigns a subscription's id, and an import checks it's a UUID before state holds it"))
	// Service accounts and their credentials, read inside an organization.
	// The provider makes neither: the platform refuses that to a
	// client_credentials login.
	sas, creds := serviceAccountsDataSourceSuffix, serviceAccountCredentialsDataSourceSuffix
	b["getServiceAccount path:orgId"] = onDataSource(serviceAccountDataSourceSuffix, "organization_id", asValue)
	b["listServiceAccountCredentials path:orgId"] = onDataSource(creds, "organization_id", asValue)
	b["listServiceAccountCredentials query:status"] = notConfigurable(notSent)
	b["listRoles path:orgId"] = onDataSource(rolesDataSourceSuffix, "organization_id", asValue)
	// The accounts data source's filters reach the account list, and the
	// credentials one's reach it too when it names no account. Both take
	// organization_id and scope from the same helpers.
	b["listServiceAccounts path:orgId"] = onDataSource(sas, "organization_id", asValue)
	b["listServiceAccounts query:scope"] = onDataSource(sas, "scope", asValue)
	b["listServiceAccounts query:status"] = notConfigurable(notSent)

	sg := securityGroupResourceSuffix
	b["createSecurityGroup body.id"] = onResource(sg, "id", asValue)
	b["updateSecurityGroup body"] = notConfigurable(nonEmptyPatch)
	for _, dir := range []string{"ingress", "egress"} {
		for spec, attr := range map[string]string{
			"name": "name", "protocol": "protocol", "action": "action",
			"sourcePrefix": "source_prefix", "destinationPrefix": "destination_prefix",
			"sourcePortRange": "source_port_range", "destinationPortRange": "destination_port_range",
		} {
			binding := onResource(sg, dir+"."+attr, asValue)
			if strings.HasSuffix(spec, "Prefix") {
				binding.stricter = "a prefix is a CIDR with its host bits clear, which the contract " +
					"describes and bounds only by length, at most the 43 characters of an IPv6 one"
				binding.accepts = []string{"10.0.0.0/8", "0.0.0.0/0", "2001:db8::/32"}
			}
			each([]string{"createSecurityGroup", "updateSecurityGroup"}, "body.rules."+dir+"[]."+spec, binding)
		}
	}

	// Data sources: path and query parameters a configuration fills in.
	b["getClusterTypeVersion path:versionId"] = onDataSource(clusterTypeVersionDataSourceSuffix, "version_id", asValue)
	b["getInstance path:id"] = onDataSource(instanceDataSourceSuffix, "id", asValue)
	b["getOrganization path:id"] = onDataSource(organizationDataSourceSuffix, "id", asValue)
	b["listClusterTypeVersions query:status"] = onDataSource(clusterTypeVersionsDataSourceSuffix, "status", asValue)
	b["listOrganizations query:status"] = onDataSource(organizationsDataSourceSuffix, "status", asValue)
	b["listSecurityGroupsEffective query:objectKind"] = onDataSource(effectiveSecurityGroupsDataSourceSuffix, "object_kind", asValue)
	b["listSecurityGroupsEffective query:objectId"] = onDataSource(effectiveSecurityGroupsDataSourceSuffix, "object_id", asValue)
	b["listClusterRuntimeEvents query:level[]"] = notConfigurable("the provider asks for `warn` and `error` itself, " +
		"to explain a failed cluster, and no configuration sets the levels")
	for _, loc := range []string{"listClusterRuntimeEvents query:since", "listClusterRuntimeEvents query:stage"} {
		b[loc] = notConfigurable(notSent)
	}
	b["listProjects query:search"] = notConfigurable("the provider searches only with a project id, which is at most 63 characters")
	for _, loc := range []string{
		"listOrganizations query:search", "listOrganizations query:orderBy", "listOrganizations query:sortOrder",
		"listProjects query:status", "listNetworks query:type", "listNetworks query:owner",
		"listSecurityGroups query:state", "listClusters query:state", "listClusters query:health",
		"listClusters query:search", "listFileStores query:state", "listInstances query:instanceGroupUuid", "listInstances query:nodePoolUuid",
		"listInstances query:reservationUuid", "listInstances query:instanceId[]", "listInstances query:inStates[]",
		"listVPCs query:owner", "listVPCs query:ownerId", "listVPCs query:ownerKind",
	} {
		b[loc] = notConfigurable(notSent)
	}
	return b
}()

// pagingParameters are bound by name wherever they appear: the provider walks
// every list itself, and no configuration sets how.
var pagingParameters = []string{"query:offset", "query:limit", "query:pageSize"}

// bindingFor is a constraint's binding, and false when it has none.
func bindingFor(c specConstraint) (specBinding, bool) {
	if slices.Contains(pagingParameters, c.loc) {
		return notConfigurable(paging), true
	}
	b, ok := specBindings[c.key()]
	return b, ok
}

// providerOperations is every operation the provider calls, read from its
// source: a call through the client is the only way it sends one.
func providerOperations(t *testing.T) []string {
	t.Helper()
	// The typed call, or the raw one a resource makes where the typed parser
	// would drop a body that carries a secret.
	calls := regexp.MustCompile(`\.Client\.([A-Z]\w*?)(?:WithResponse)?\(ctx`)
	seen := map[string]bool{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f) //nolint:gosec // a file in this package
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range calls.FindAllStringSubmatch(string(raw), -1) {
			seen[strings.ToLower(m[1][:1])+m[1][1:]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for op := range seen {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

// specDocument is the contract the client in internal/k0rdent is generated
// from: the public operations the provider calls.
func specDocument(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "k0rdent", "spec", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read the client's spec: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the client's spec: %v", err)
	}
	return doc
}

// asMap and asList read a decoded YAML node as the shape it should have, and
// as empty when it hasn't.
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any) //nolint:errcheck // a node of another shape reads as empty
	return m
}

func asList(v any) []any {
	l, _ := v.([]any) //nolint:errcheck // as asMap
	return l
}

// resolve follows $ref until it reaches a schema.
func resolve(doc map[string]any, node any) map[string]any {
	for range 32 {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		var cur any = doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			cur = asMap(cur)[part]
		}
		node = cur
	}
	return nil
}

// specConstraints lists every constrained value in the operations named.
func specConstraints(t *testing.T, doc map[string]any, ops []string) []specConstraint {
	t.Helper()
	byID := map[string]map[string]any{}
	pathParams := map[string][]any{}
	for _, item := range asMap(doc["paths"]) {
		item := asMap(item)
		shared := asList(item["parameters"])
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			if op, ok := item[method].(map[string]any); ok {
				id := fmt.Sprint(op["operationId"])
				byID[id] = op
				pathParams[id] = shared
			}
		}
	}

	var out []specConstraint
	var walk func(op, loc string, node any, depth int)
	walk = func(op, loc string, node any, depth int) {
		s := resolve(doc, node)
		if s == nil || depth > 16 || s["readOnly"] == true {
			return
		}
		if all, ok := s["allOf"].([]any); ok {
			for _, sub := range all {
				walk(op, loc, sub, depth+1)
			}
		}
		for _, key := range []string{"oneOf", "anyOf"} {
			if subs, ok := s[key].([]any); ok {
				for i, sub := range subs {
					walk(op, fmt.Sprintf("%s|%s%d", loc, key, i), sub, depth+1)
				}
			}
		}
		constrained := map[string]any{}
		for _, k := range constraintKeywords {
			if v, ok := s[k]; ok {
				constrained[k] = v
			}
		}
		if len(constrained) > 0 {
			out = append(out, specConstraint{op: op, loc: loc, schema: constrained})
		}
		if props, ok := s["properties"].(map[string]any); ok {
			for name, p := range props {
				walk(op, loc+"."+name, p, depth+1)
			}
		}
		if items, ok := s["items"]; ok {
			walk(op, loc+"[]", items, depth+1)
		}
		if extra, ok := s["additionalProperties"].(map[string]any); ok {
			walk(op, loc+"{}", extra, depth+1)
		}
	}

	for _, id := range ops {
		op, ok := byID[id]
		if !ok {
			t.Errorf("the provider calls %s, which the client's spec doesn't have", id)
			continue
		}
		params := asList(op["parameters"])
		for _, p := range append(slices.Clone(pathParams[id]), params...) {
			p := resolve(doc, p)
			walk(id, fmt.Sprintf("%s:%s", p["in"], p["name"]), p["schema"], 0)
		}
		if body := resolve(doc, op["requestBody"]); body != nil {
			for _, media := range asMap(body["content"]) {
				walk(id, "body", asMap(media)["schema"], 0)
			}
		}
	}
	// One entry per location: allOf can name the same one twice.
	merged := map[string]specConstraint{}
	for _, c := range out {
		if prev, ok := merged[c.key()]; ok {
			for k, v := range c.schema {
				prev.schema[k] = v
			}
			continue
		}
		merged[c.key()] = c
	}
	out = out[:0]
	for _, c := range merged {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func TestEverySpecConstraintIsCheckedAtPlanTime(t *testing.T) {
	doc := specDocument(t)
	constraints := specConstraints(t, doc, providerOperations(t))
	if len(constraints) < 50 {
		t.Fatalf("found %d constrained values; the walk has stopped reaching the request bodies", len(constraints))
	}
	attrs := registeredAttributes(t)
	used := map[string]bool{}
	for _, c := range constraints {
		b, ok := bindingFor(c)
		if !ok {
			t.Errorf("%s: the contract constrains it (%s) and nothing checks it at plan time: bind it in "+
				"specBindings, or say there why no configuration reaches it", c.key(), describeConstraint(c.schema))
			continue
		}
		used[c.key()] = true
		if b.reason != "" {
			continue
		}
		a, ok := attrs[b.kind+" "+ResourceName(b.suffix)+"."+b.attr]
		if !ok {
			t.Errorf("%s is bound to %s %s.%s, which doesn't exist", c.key(), b.kind, ResourceName(b.suffix), b.attr)
			continue
		}
		for _, failure := range probeConstraint(c, b, a) {
			t.Errorf("%s (%s) on %s %s.%s: %s", c.key(), describeConstraint(c.schema), b.kind, ResourceName(b.suffix), b.attr, failure)
		}
	}
	for key := range specBindings {
		if !used[key] {
			t.Errorf("specBindings has %q, which the contract no longer constrains in an operation the provider calls", key)
		}
	}
}

func describeConstraint(s map[string]any) string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %v", k, s[k])
	}
	return strings.Join(parts, ", ")
}

// registeredAttributes indexes every attribute of every resource and data
// source as "<kind> <type>.<dotted path>".
func registeredAttributes(t *testing.T) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, s := range identifierSurfaces(t) {
		var walk func(v reflect.Value, prefix string)
		walk = func(v reflect.Value, prefix string) {
			for _, method := range []string{"GetAttributes", "GetBlocks"} {
				get := v.MethodByName(method)
				if !get.IsValid() {
					continue
				}
				for it := get.Call(nil)[0].MapRange(); it.Next(); {
					name := prefix + it.Key().String()
					child := it.Value().Elem()
					out[s.kind+" "+s.name+"."+name] = child.Interface()
					if nested := child.MethodByName("GetNestedObject"); nested.IsValid() {
						walk(nested.Call(nil)[0], name+".")
					}
				}
			}
		}
		walk(reflect.ValueOf(s.schema), "")
	}
	return out
}

// probeConstraint checks an attribute's validators against one constraint:
// each value the contract refuses has to be refused, and each it accepts
// has to be accepted. It returns what didn't hold.
func probeConstraint(c specConstraint, b specBinding, a any) []string {
	var failures []string
	refuse, accept := probes(c.schema, b.mode)
	if b.stricter != "" {
		accept = accept[:0]
		for _, v := range b.accepts {
			accept = append(accept, probe{kind: "string", str: v})
		}
	}
	if len(refuse) == 0 {
		failures = append(failures, "no value the contract refuses could be made to probe it with")
	}
	for _, p := range refuse {
		ok, err := runValidators(a, b, p)
		if err != nil {
			return append(failures, err.Error())
		}
		if ok {
			failures = append(failures, fmt.Sprintf("accepts %s, which the contract refuses", p))
		}
	}
	for _, p := range accept {
		ok, err := runValidators(a, b, p)
		if err != nil {
			return append(failures, err.Error())
		}
		if !ok {
			failures = append(failures, fmt.Sprintf("refuses %s, which the contract accepts", p))
		}
	}
	return failures
}

// probe is one value to try: a string, a number, or a collection size.
type probe struct {
	str  string
	num  int64
	size int
	kind string // "string", "number" or "size"
}

func (p probe) String() string {
	switch p.kind {
	case "number":
		return strconv.FormatInt(p.num, 10)
	case "size":
		return fmt.Sprintf("%d elements", p.size)
	}
	if len(p.str) > 24 {
		return fmt.Sprintf("a %d-character string", len(p.str))
	}
	return strconv.Quote(p.str)
}

// probes makes the values to try from a constraint's keywords.
func probes(s map[string]any, mode probeMode) (refuse, accept []probe) {
	if mode == asSize {
		lo, hasLo := intKeyword(s, "minItems")
		hi, hasHi := intKeyword(s, "maxItems")
		if hasLo && lo > 0 {
			refuse = append(refuse, probe{kind: "size", size: int(lo) - 1})
			accept = append(accept, probe{kind: "size", size: int(lo)})
		}
		if hasHi {
			refuse = append(refuse, probe{kind: "size", size: int(hi) + 1})
			accept = append(accept, probe{kind: "size", size: int(hi)})
		}
		return refuse, accept
	}

	if _, numeric := s["minimum"]; numeric || s["format"] == "int32" || s["format"] == "int64" {
		lo, hasLo := intKeyword(s, "minimum")
		hi, hasHi := intKeyword(s, "maximum")
		if s["format"] == "int32" {
			if !hasHi {
				hi, hasHi = math.MaxInt32, true
			}
			if !hasLo {
				lo, hasLo = math.MinInt32, true
			}
		}
		if hasLo {
			refuse = append(refuse, probe{kind: "number", num: lo - 1})
			accept = append(accept, probe{kind: "number", num: lo})
		}
		if hasHi {
			refuse = append(refuse, probe{kind: "number", num: hi + 1})
			accept = append(accept, probe{kind: "number", num: hi})
		}
		return refuse, accept
	}

	ok := stringAcceptor(s)
	var candidates []string
	if enum, isEnum := s["enum"].([]any); isEnum {
		for _, e := range enum {
			accept = append(accept, probe{kind: "string", str: fmt.Sprint(e)})
		}
		candidates = append(candidates, "not-in-the-enum", "", strings.ToUpper(fmt.Sprint(enum[0])))
	} else if sample, found := sampleString(s); found {
		accept = append(accept, probe{kind: "string", str: sample})
		if hi, has := intKeyword(s, "maxLength"); has {
			if long, found := padTo(sample, int(hi), ok); found {
				accept = append(accept, probe{kind: "string", str: long})
			}
			if long, found := padTo(sample, int(hi)+1, func(v string) bool { return ok(v[:len(v)-1]) || ok(v[1:]) }); found {
				candidates = append(candidates, long)
			}
		}
		candidates = append(candidates, sample+" ", " "+sample, sample+"\n", strings.ToUpper(sample)+"Z", sample+"/x")
	}
	candidates = append(candidates, probeCorpus...)
	seen := map[string]bool{}
	for _, v := range candidates {
		if !seen[v] && !ok(v) {
			refuse = append(refuse, probe{kind: "string", str: v})
		}
		seen[v] = true
	}
	return refuse, accept
}

// probeCorpus is a fixed set of values that break one common rule or another:
// empty, case, separators, path characters, signs and leading zeroes.
var probeCorpus = []string{
	"", " ", "-", "A", "Aa", "a-", "-a", "1a", "a_b", "a.b", "..", ".", "a b", "a/b", "/", "//", "/a//b",
	"+80", "080", "99999", "1-", "-1", "0x", "0xZZ", "auto1", "00:00", "zz:zz:zz:zz:zz:zz",
	"not-a-uuid", "00000000-0000-0000-0000-00000000000g", "inst-", "inst-XYZ",
}

// stringAcceptor reports whether the contract accepts a string under these
// keywords.
func stringAcceptor(s map[string]any) func(string) bool {
	var re *regexp.Regexp
	if p, ok := s["pattern"].(string); ok {
		re = regexp.MustCompile(p)
	}
	enum, isEnum := s["enum"].([]any)
	lo, hasLo := intKeyword(s, "minLength")
	hi, hasHi := intKeyword(s, "maxLength")
	return func(v string) bool {
		n := int64(len([]rune(v)))
		switch {
		case hasLo && n < lo, hasHi && n > hi:
			return false
		case re != nil && !re.MatchString(v):
			return false
		case isEnum && !slices.ContainsFunc(enum, func(e any) bool { return fmt.Sprint(e) == v }):
			return false
		case s["format"] == "uuid" && !uuidPattern.MatchString(v):
			return false
		case s["format"] == "date-time" && !isDateTime(v):
			return false
		}
		return true
	}
}

// isDateTime is the contract's `format: date-time`, which is RFC 3339.
func isDateTime(v string) bool {
	_, err := time.Parse(time.RFC3339, v)
	return err == nil
}

// sampleString makes a short value the keywords accept: the shortest match of
// the pattern, padded to minLength.
func sampleString(s map[string]any) (string, bool) {
	ok := stringAcceptor(s)
	sample := "a"
	switch s["format"] {
	case "uuid":
		sample = "123e4567-e89b-42d3-a456-426614174000"
	case "date-time":
		sample = "2027-01-31T00:00:00Z"
	}
	if p, isPattern := s["pattern"].(string); isPattern {
		re, err := syntax.Parse(p, syntax.Perl)
		if err != nil {
			return "", false
		}
		sample = shortestMatch(re.Simplify())
	}
	if lo, has := intKeyword(s, "minLength"); has && int64(len(sample)) < lo {
		if padded, found := padTo(sample, int(lo), ok); found {
			sample = padded
		}
	}
	return sample, ok(sample)
}

// shortestMatch writes the shortest string a parsed pattern matches, taking
// the first choice at each alternative and a letter where a class allows one.
func shortestMatch(re *syntax.Regexp) string {
	var b strings.Builder
	var walk func(*syntax.Regexp)
	walk = func(re *syntax.Regexp) {
		switch re.Op {
		case syntax.OpLiteral:
			b.WriteString(string(re.Rune))
		case syntax.OpCharClass:
			b.WriteRune(classRune(re.Rune))
		case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
			b.WriteRune('a')
		case syntax.OpCapture:
			walk(re.Sub[0])
		case syntax.OpConcat:
			for _, sub := range re.Sub {
				walk(sub)
			}
		case syntax.OpAlternate:
			walk(re.Sub[0])
		case syntax.OpPlus:
			walk(re.Sub[0])
		case syntax.OpRepeat:
			for range re.Min {
				walk(re.Sub[0])
			}
		}
	}
	walk(re)
	return b.String()
}

// classRune picks a lower-case letter from a character class when it holds
// one, then a digit, then whatever it starts with.
func classRune(ranges []rune) rune {
	for _, want := range []rune{'a', '0'} {
		for i := 0; i+1 < len(ranges); i += 2 {
			if ranges[i] <= want && want <= ranges[i+1] {
				return want
			}
		}
	}
	return ranges[0]
}

// padTo lengthens a sample to n characters with a filler the keywords still
// accept, trying a few.
func padTo(sample string, n int, ok func(string) bool) (string, bool) {
	if len(sample) > n {
		return "", false
	}
	for _, filler := range []string{"a", "0", "x", "-a"} {
		v := sample + strings.Repeat(filler, n)
		v = v[:n]
		if ok(v) {
			return v, true
		}
		v = strings.Repeat(filler, n)[:n-len(sample)] + sample
		if ok(v) {
			return v, true
		}
	}
	return "", false
}

func intKeyword(s map[string]any, k string) (int64, bool) {
	switch v := s[k].(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	}
	return 0, false
}

// runValidators runs an attribute's own validators on one probe, placed as
// the binding says, and reports whether they accepted it.
func runValidators(a any, b specBinding, p probe) (bool, error) {
	ctx := context.Background()
	at := path.Root("probe")
	str := p.str
	if b.wrap != nil {
		str = b.wrap(str)
	}

	switch b.mode {
	case asValue:
		switch v := a.(type) {
		case interface{ StringValidators() []validator.String }:
			var resp validator.StringResponse
			for _, val := range v.StringValidators() {
				val.ValidateString(ctx, validator.StringRequest{Path: at, ConfigValue: types.StringValue(str)}, &resp)
			}
			return !resp.Diagnostics.HasError(), nil
		case interface{ Int64Validators() []validator.Int64 }:
			var resp validator.Int64Response
			for _, val := range v.Int64Validators() {
				val.ValidateInt64(ctx, validator.Int64Request{Path: at, ConfigValue: types.Int64Value(p.num)}, &resp)
			}
			return !resp.Diagnostics.HasError(), nil
		}
	case asElement, asKey, asSize:
		typed, isTyped := a.(interface{ GetType() attr.Type })
		if !isTyped {
			return false, fmt.Errorf("%T has no type", a)
		}
		value, err := collectionOf(typed.GetType(), b.mode, p)
		if err != nil {
			return false, err
		}
		switch v := a.(type) {
		case interface{ ListValidators() []validator.List }:
			list, isList := value.(types.List)
			if !isList {
				return false, fmt.Errorf("built a %T for a list", value)
			}
			var resp validator.ListResponse
			for _, val := range v.ListValidators() {
				val.ValidateList(ctx, validator.ListRequest{Path: at, ConfigValue: list}, &resp)
			}
			return !resp.Diagnostics.HasError(), nil
		case interface{ SetValidators() []validator.Set }:
			set, isSet := value.(types.Set)
			if !isSet {
				return false, fmt.Errorf("built a %T for a set", value)
			}
			var resp validator.SetResponse
			for _, val := range v.SetValidators() {
				val.ValidateSet(ctx, validator.SetRequest{Path: at, ConfigValue: set}, &resp)
			}
			return !resp.Diagnostics.HasError(), nil
		case interface{ MapValidators() []validator.Map }:
			m, isMap := value.(types.Map)
			if !isMap {
				return false, fmt.Errorf("built a %T for a map", value)
			}
			var resp validator.MapResponse
			for _, val := range v.MapValidators() {
				val.ValidateMap(ctx, validator.MapRequest{Path: at, ConfigValue: m}, &resp)
			}
			return !resp.Diagnostics.HasError(), nil
		}
	}
	return false, fmt.Errorf("can't probe a %T as %v", a, b.mode)
}

// collectionOf builds a list, set or map for a probe: one element or key
// holding the probe, or as many distinct elements as the probe's size.
func collectionOf(typ attr.Type, mode probeMode, p probe) (attr.Value, error) {
	var elem attr.Type
	switch t := typ.(type) {
	case types.ListType:
		elem = t.ElemType
	case types.SetType:
		elem = t.ElemType
	case types.MapType:
		elem = t.ElemType
	default:
		return nil, fmt.Errorf("%T is not a collection", typ)
	}
	n := 1
	if mode == asSize {
		n = p.size
	}
	elems := make([]attr.Value, n)
	keys := make([]string, n)
	for i := range n {
		keys[i] = "k" + strconv.Itoa(i)
		v, err := distinctElement(elem, i)
		if err != nil {
			return nil, err
		}
		elems[i] = v
	}
	switch mode {
	case asElement:
		e, err := elementFromString(elem, p.str)
		if err != nil {
			return nil, err
		}
		elems[0] = e
	case asKey:
		keys[0] = p.str
	}

	switch t := typ.(type) {
	case types.ListType:
		return types.ListValueMust(t.ElemType, elems), nil
	case types.SetType:
		return types.SetValueMust(t.ElemType, elems), nil
	default:
		m := make(map[string]attr.Value, n)
		for i, k := range keys {
			m[k] = elems[i]
		}
		return types.MapValueMust(elem, m), nil
	}
}

// distinctElement is the ith of a run of distinct, ordinary elements: a short
// id for a string, and for an object every attribute null but the first
// string one, which carries the id.
func distinctElement(elem attr.Type, i int) (attr.Value, error) {
	id := "a" + strconv.Itoa(i)
	return elementFromString(elem, id)
}

func elementFromString(elem attr.Type, s string) (attr.Value, error) {
	switch t := elem.(type) {
	case basetypes.StringType:
		return types.StringValue(s), nil
	case types.ObjectType:
		names := make([]string, 0, len(t.AttrTypes))
		for name := range t.AttrTypes {
			names = append(names, name)
		}
		sort.Strings(names)
		vals := map[string]attr.Value{}
		set := false
		for _, name := range names {
			at := t.AttrTypes[name]
			if !set && at.Equal(types.StringType) {
				vals[name] = types.StringValue(s)
				set = true
				continue
			}
			vals[name] = nullOf(at)
		}
		return types.ObjectValueMust(t.AttrTypes, vals), nil
	}
	return nil, fmt.Errorf("can't build an element of %T", elem)
}

func nullOf(t attr.Type) attr.Value {
	switch t := t.(type) {
	case types.ListType:
		return types.ListNull(t.ElemType)
	case types.SetType:
		return types.SetNull(t.ElemType)
	case types.MapType:
		return types.MapNull(t.ElemType)
	case types.ObjectType:
		return types.ObjectNull(t.AttrTypes)
	case basetypes.Int64Type:
		return types.Int64Null()
	case basetypes.BoolType:
		return types.BoolNull()
	}
	return types.StringNull()
}

// The probes have to be able to fail, or every binding would pass. An
// attribute with no validator fails each kind of constraint, and one with
// the matching validator passes.
func TestTheSpecProbesCatchAMissingValidator(t *testing.T) {
	id := specConstraint{op: "op", loc: "body.id", schema: map[string]any{
		"pattern": `^[a-z]([-a-z0-9]*[a-z0-9])?$`, "minLength": 1, "maxLength": 63,
	}}
	bare := onResource("x", "id", asValue)
	if got := probeConstraint(id, bare, rschema.StringAttribute{Optional: true}); len(got) == 0 {
		t.Error("an id with no validator passed")
	}
	if got := probeConstraint(id, bare, rschema.StringAttribute{Optional: true, Validators: idValidators()}); len(got) > 0 {
		t.Errorf("idValidators failed the id probes: %q", got)
	}

	count := specConstraint{op: "op", loc: "body.size", schema: map[string]any{"minimum": 0, "format": "int32"}}
	if got := probeConstraint(count, bare, rschema.Int64Attribute{Optional: true}); len(got) != 2 {
		t.Errorf("an unchecked int32 gave %q, want -1 and 2147483648 refused", got)
	}

	enum := specConstraint{op: "op", loc: "body.kind", schema: map[string]any{"enum": []any{"ro", "rw"}}}
	if got := probeConstraint(enum, bare, rschema.StringAttribute{Optional: true}); len(got) == 0 {
		t.Error("an enum with no validator passed")
	}

	size := specConstraint{op: "op", loc: "body.groups", schema: map[string]any{"maxItems": 2}}
	list := rschema.ListAttribute{Optional: true, ElementType: types.StringType}
	if got := probeConstraint(size, onResource("x", "groups", asSize), list); len(got) != 1 {
		t.Errorf("an unbounded list gave %q, want 3 elements refused", got)
	}
}

// The sample a pattern's probes start from has to match it, or the accept
// side would test nothing.
func TestShortestMatchMatches(t *testing.T) {
	for _, p := range []string{
		k0rdent.ClusterCreateRequestIdPattern, k0rdent.GetInstancePathIdPattern, k0rdent.ConsumerNetworkGuidPattern,
		k0rdent.ConsumerNetworkMacAddressPattern, k0rdent.NetworkCreateRequestEVPNVlanIdPattern,
		k0rdent.SshUserUsernamePattern, k0rdent.ConsumerMountPathPattern, k0rdent.SecurityGroupRuleSourcePortRangePattern,
	} {
		sample, ok := sampleString(map[string]any{"pattern": p})
		if !ok || !regexp.MustCompile(p).MatchString(sample) {
			t.Errorf("%s: sample %q doesn't match", p, sample)
		}
	}
}
