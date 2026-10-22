package acceptance

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// churns is the marker the fake looks for in an object's id. An object that
// carries it reads back differently every time in exactly the fields the
// contract says the platform moves on its own (see churn.go in the fake), so
// a refresh sees what a refresh of a live object would see.
const churns = "fake-churns"

// perpetualDiffCase is one managed resource under the perpetual-diff check.
type perpetualDiffCase struct {
	// kind names the subtest, so a failure says which resource drifted.
	kind string
	// config is the resource and whatever it needs to exist, with the
	// resource under test at `<type>.test`.
	config string
	// addr is the resource under test.
	addr string
	// read is the object the fake churns, built from the resource's state,
	// so the check can show the fake really did vary it.
	read func(attrs map[string]string) string
	// gone is the CheckDestroy for the resource under test.
	gone resource.TestCheckFunc
}

// perpetualDiffCases is every managed resource the provider registers. A
// resource added without a case here is not checked, and
// TestPerpetualDiffCoversEveryResource says so.
func perpetualDiffCases() []perpetualDiffCase {
	const (
		cluster = "acc-cluster-" + churns
		group   = "acc-hosts-" + churns
		store   = "acc-store-" + churns
		network = "acc-net-" + churns
		subnet  = "acc-subnet-" + churns
		sg      = "acc-sg-" + churns
		peer    = "acc-peer-" + churns
		owner   = "acc-peer-owner-" + churns
		binding = "acc-bind-owner-" + churns
		project = "acc-project-" + churns
	)
	remote := "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
	vpcPath := func(a map[string]string) string {
		return fmt.Sprintf("/v1/regions/%s/projects/%s/networking/vpcs/%s", a["region"], a["project"], a["vpc"])
	}

	return []perpetualDiffCase{
		{
			kind: "kubernetes_cluster", config: clusterConfig(cluster, 2), addr: clusterAddr,
			read: clusterPath, gone: checkGone(clusterType, clusterPath),
		},
		{
			kind: "instance_group", config: instanceGroupConfig(group, 2), addr: instanceGroupAddr,
			read: instanceGroupPath, gone: checkGone(instanceGroupType, instanceGroupPath),
		},
		{
			// An instance-group consumer, because that is the entry whose
			// resolved addresses the platform reports, and the group has two
			// hosts so the order of the addresses can move.
			kind: "file_store", config: groupStoreConfig(store, store+"-hosts", 2), addr: fileStoreAddr,
			read: fileStorePath, gone: checkGone(fileStoreType, fileStorePath),
		},
		{
			kind: "network", config: evpnConfig(network, "acceptance"), addr: networkAddr,
			read: networkPath, gone: checkGone(networkType, networkPath),
		},
		{
			kind: "subnet", config: subnetConfig(subnet+"-net", subnet), addr: subnetAddr,
			read: subnetPath, gone: checkGone(subnetType, subnetPath),
		},
		{
			kind: "security_group", config: securityGroupConfig(sg, true), addr: securityGroupAddr,
			read: securityGroupPath, gone: checkGone(securityGroupType, securityGroupPath),
		},
		{
			kind: "vpc_peering", config: peeringConfig(owner, peer, remote), addr: peeringAddr,
			read: peeringPath, gone: checkGone(peeringType, peeringPath),
		},
		{
			// The binding is a property of the VPC, so the VPC is what churns.
			// Its destroy is a write rather than a delete, and the cluster that
			// owns the VPC is what checkGone can ask about.
			kind: "vpc_security_groups", config: vpcBindingConfig(binding, binding+"-sg", true), addr: vpcBindingAddr,
			read: vpcPath, gone: checkGone(clusterType, clusterPath),
		},
		{
			kind: "project", config: projectConfig(project, "acceptance", "perpetual diff"), addr: projectAddr,
			read: projectPath, gone: checkGone(projectType, projectPath),
		},
		{
			// The platform picks the id, so the endpoint carries the marker,
			// and the list is what churns: there's no read of one. A case
			// here is a bare configuration, so the secret is derived in it.
			kind: "webhook_subscription",
			config: webhookConfigFixedSecret(webhookURL(churns),
				[]string{"io.k0rdent.cluster.ready.v1", "io.k0rdent.cluster.failed.v1"}, "churn"),
			addr: webhookAddr,
			read: func(map[string]string) string { return "/v1/regions/global/notifications/webhooks" },
			gone: checkWebhookGone,
		},
	}
}

// TestAccPerpetualDiff is the perpetual-diff check: for every managed
// resource, apply, then plan with no change and expect nothing, then refresh
// and plan again and still expect nothing, while the fake varies every
// platform-computed value the live API varies between reads.
//
// terraform-plugin-testing already plans twice after every apply, once
// without a refresh and once with one, and fails on a non-empty plan. Against
// a fake that answers the same bytes every time, that is a check that cannot
// fail on the thing it is for: a value the platform moves on its own. So every
// object here carries the churn marker, the first step proves the fake really
// did answer two different bodies, and the refresh and plan are repeated so
// the varied fields go through every shape they take.
func TestAccPerpetualDiff(t *testing.T) {
	for _, c := range perpetualDiffCases() {
		t.Run(c.kind, func(t *testing.T) {
			empty := []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}
			// Each refresh has to leave state as the step before it left it.
			// That's `terraform plan -refresh-only` exiting 0, which an empty
			// plan after the refresh doesn't show: a computed value that moves
			// between reads plans nothing and is still drift.
			unchanged := &refreshOnly{}
			replan := resource.TestStep{
				Config:   c.config,
				PlanOnly: true,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PostApplyPreRefresh:  empty,
					PostApplyPostRefresh: empty,
				},
			}
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				CheckDestroy:             c.gone,
				Steps: []resource.TestStep{
					{
						Config: c.config,
						Check:  resource.ComposeAggregateTestCheckFunc(checkChurns(c.addr, c.read), unchanged.record),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PostApplyPreRefresh:  empty,
							PostApplyPostRefresh: empty,
						},
					},
					replan,
					{RefreshState: true, Check: unchanged.compare},
					replan,
					{RefreshState: true, Check: unchanged.compare},
					replan,
				},
			})
		})
	}
}

// refreshOnly asks the refresh-only plan's question of state: did a refresh
// move any attribute of a managed resource. record keeps what a step left,
// and compare fails on anything a refresh moved, naming the address and
// attribute, then keeps the refreshed state for the next refresh.
type refreshOnly struct {
	last map[string]map[string]string
}

// refreshMayMove is every attribute a refresh of a churning object is allowed
// to move, and why. Each is the platform reporting a change it really made,
// which a refresh-only plan is right to show. Anything else a refresh moves
// fails, so a churned value that leaks into state can't join this list
// unnoticed.
var refreshMayMove = map[string]string{
	clusterType + ".state": "the fake takes a settled cluster to failed and back, as the watchdog can. " +
		"A live state change is drift worth seeing",
	fileStoreType + ".etag": "the fake moves the revision on every read, standing in for a server-driven " +
		"transition. A settled store keeps its etag, and the provider needs the latest one for If-Match",
}

func (r *refreshOnly) record(s *terraform.State) error {
	r.last = map[string]map[string]string{}
	for addr, rs := range s.RootModule().Resources {
		if rs.Primary != nil && !strings.HasPrefix(addr, "data.") {
			r.last[addr] = rs.Primary.Attributes
		}
	}
	return nil
}

func (r *refreshOnly) compare(s *terraform.State) error {
	before := r.last
	if err := r.record(s); err != nil {
		return err
	}
	var moved []string
	for addr, was := range before {
		now, ok := r.last[addr]
		if !ok {
			moved = append(moved, addr+" left state")
			continue
		}
		typ, _, _ := strings.Cut(addr, ".")
		for k, v := range was {
			if _, allowed := refreshMayMove[typ+"."+k]; allowed {
				continue
			}
			if nv, ok := now[k]; !ok || nv != v {
				moved = append(moved, fmt.Sprintf("%s.%s: %q to %q", addr, k, v, nv))
			}
		}
		for k, nv := range now {
			if _, ok := was[k]; !ok {
				moved = append(moved, fmt.Sprintf("%s.%s: unset to %q", addr, k, nv))
			}
		}
	}
	for addr := range r.last {
		if _, ok := before[addr]; !ok {
			moved = append(moved, addr+" appeared in state")
		}
	}
	if len(moved) > 0 {
		slices.Sort(moved)
		return fmt.Errorf("a refresh with nothing changed moved state, so `terraform plan -refresh-only` "+
			"would report drift:\n  %s", strings.Join(moved, "\n  "))
	}
	return nil
}

// checkChurns reads the object twice straight from the fake and fails if the
// two bodies are the same. Without it a typo in the marker, or a fake that
// stopped varying anything, would leave every step above passing against
// identical answers, which is the vacuous check this test exists to replace.
func checkChurns(addr string, pathOf func(map[string]string) string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[addr]
		if !ok {
			return fmt.Errorf("%s is not in state", addr)
		}
		path := pathOf(rs.Primary.Attributes)
		var bodies [2][]byte
		for i := range bodies {
			status, body, err := readFake(path)
			if err != nil {
				return err
			}
			if status != http.StatusOK {
				return fmt.Errorf("reading %s answered %d", path, status)
			}
			bodies[i] = body
		}
		if bytes.Equal(bodies[0], bodies[1]) {
			return fmt.Errorf("the fake answered %s with the same body twice, so the plans after it "+
				"checked nothing a live platform would have moved:\n%s", path, bodies[0])
		}
		return nil
	}
}

// TestPerpetualDiffCoversEveryResource fails when a managed resource is
// registered without a perpetual-diff case, so a new resource can't skip the
// check by being added after it was written. It needs no fake.
func TestPerpetualDiffCoversEveryResource(t *testing.T) {
	ctx := context.Background()
	var registered []string
	for _, newResource := range provider.New("acc")().Resources(ctx) {
		var meta fwresource.MetadataResponse
		newResource().Metadata(ctx, fwresource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &meta)
		registered = append(registered, strings.TrimPrefix(meta.TypeName, provider.ProviderTypeName+"_"))
	}
	var checked []string
	for _, c := range perpetualDiffCases() {
		checked = append(checked, c.kind)
	}
	slices.Sort(registered)
	slices.Sort(checked)
	if !slices.Equal(registered, checked) {
		t.Errorf("the perpetual-diff check covers %v, and the provider registers %v", checked, registered)
	}
}
