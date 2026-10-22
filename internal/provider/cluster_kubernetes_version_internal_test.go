package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func planned(t *testing.T, resp resource.ModifyPlanResponse, name string) types.String {
	t.Helper()
	var v types.String
	if d := resp.Plan.GetAttribute(context.Background(), path.Root(name), &v); d.HasError() {
		t.Fatalf("read plan %s: %v", name, d)
	}
	return v
}

// State reports the Kubernetes version the bound ClusterType version carries,
// and an upgrade moves it to the version reached.
func TestTheClusterReportsItsBindingsKubernetesVersion(t *testing.T) {
	api := &upgradeAPI{bound: 1, state: "active", workers: 2}
	resp := runUpgrade(t, api, 4, 2)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics.Errors())
	}
	if got := stateAttr(t, resp.State, "kubernetes_version"); got != k8sAt(4) {
		t.Errorf("kubernetes_version = %q, want %q", got, k8sAt(4))
	}
}

// A failed step leaves the version the cluster is still on.
func TestAFailedStepReportsTheKubernetesVersionItStayedOn(t *testing.T) {
	api := &upgradeAPI{bound: 2, state: "active", workers: 2, failInto: 5}
	resp := runUpgrade(t, api, 5, 2)
	if got := stateAttr(t, resp.State, "kubernetes_version"); got != k8sAt(4) {
		t.Errorf("kubernetes_version = %q, want %q", got, k8sAt(4))
	}
}

// The plan says what an upgrade does to the Kubernetes version, read from the
// version the route ends on, so the apply reports exactly what was planned.
func TestAnUpgradePlansTheKubernetesVersionItReaches(t *testing.T) {
	server := (&upgradeAPI{bound: 1, state: "active"}).serve(t)
	resp := planUpgrade(t, testClusterMeta(t, server.URL, time.Minute), verRef(1), verRef(4), "active")
	if resp.Diagnostics.HasError() {
		t.Fatalf("plan: %v", resp.Diagnostics)
	}
	if got := planned(t, resp, "kubernetes_version"); got.ValueString() != k8sAt(4) {
		t.Errorf("kubernetes_version planned %v, want %q", got, k8sAt(4))
	}
}

// A plan that can't read the route, or doesn't know the version yet, can't
// promise the Kubernetes version either.
func TestAnUpgradeThePlanCannotReadPlansTheKubernetesVersionUnknown(t *testing.T) {
	prior := clusterValues(verRef(1), verRef(1), 2, "active")
	prior["kubernetes_version"] = str(k8sAt(1))
	for name, configured := range map[string]tftypes.Value{
		"no client":             str(verRef(4)),
		"cluster_type at apply": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	} {
		t.Run(name, func(t *testing.T) {
			next := clusterValues(verRef(1), verRef(1), 2, "active")
			next["cluster_type"] = configured
			next["kubernetes_version"] = str(k8sAt(1))
			resp := planWith(t, nil, prior, next)
			if got := planned(t, resp, "kubernetes_version"); !got.IsUnknown() {
				t.Errorf("kubernetes_version planned %v, want unknown", got)
			}
		})
	}
}

// A change that isn't an upgrade keeps the version it has.
func TestAPlanThatIsNotAnUpgradeKeepsTheKubernetesVersion(t *testing.T) {
	server := (&upgradeAPI{bound: 1, state: "active"}).serve(t)
	prior := clusterValues(verRef(1), verRef(1), 2, "active")
	prior["kubernetes_version"] = str(k8sAt(1))
	next := clusterValues(verRef(1), verRef(1), 3, "active")
	next["kubernetes_version"] = str(k8sAt(1))
	resp := planWith(t, testClusterMeta(t, server.URL, time.Minute), prior, next)
	if got := planned(t, resp, "kubernetes_version"); got.ValueString() != k8sAt(1) {
		t.Errorf("kubernetes_version planned %v", got)
	}
}

// A version with no Kubernetes configuration, one the platform no longer
// finds, and one that can't be read all read null. The last says so in a
// warning: it's a reading, and losing it is no reason to fail a refresh.
func TestTheKubernetesVersionWhenTheBindingSaysNone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		api   *upgradeAPI
		warns bool
	}{
		{"BMaaS-only", &upgradeAPI{bound: 1, state: "active", workers: 2, bmaas: map[int]bool{1: true}}, false},
		{"withdrawn", &upgradeAPI{bound: 1, state: "active", workers: 2, versionStatus: map[int]int{1: http.StatusNotFound}}, false},
		{"unreadable", &upgradeAPI{bound: 1, state: "active", workers: 2, versionStatus: map[int]int{1: http.StatusInternalServerError}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := refresh(t, tc.api, clusterValues(verRef(1), verRef(1), 2, "active"))
			if resp.Diagnostics.HasError() || (resp.Diagnostics.WarningsCount() > 0) != tc.warns {
				t.Fatalf("diagnostics: %v", resp.Diagnostics)
			}
			if got := stateValue(t, resp.State, "kubernetes_version"); !got.IsNull() {
				t.Errorf("kubernetes_version = %v, want null", got)
			}
		})
	}
}

func refresh(t *testing.T, api *upgradeAPI, prior map[string]tftypes.Value) resource.ReadResponse {
	t.Helper()
	server := api.serve(t)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	raw, s := testValue(t, r, prior)
	resp := resource.ReadResponse{State: tfsdk.State{Raw: raw, Schema: s.Schema}}
	r.Read(context.Background(), resource.ReadRequest{State: tfsdk.State{Raw: raw, Schema: s.Schema}}, &resp)
	return resp
}

func stateValue(t *testing.T, st tfsdk.State, name string) types.String {
	t.Helper()
	var v types.String
	if d := st.GetAttribute(context.Background(), path.Root(name), &v); d.HasError() {
		t.Fatalf("read %s: %v", name, d)
	}
	return v
}

// A ClusterType version is a snapshot that doesn't change, so a refresh of a
// cluster still bound to the same one keeps the version it recorded and
// doesn't ask again, and a cluster type read that would fail can't touch it.
func TestARefreshKeepsTheKubernetesVersionOfAnUnchangedBinding(t *testing.T) {
	api := &upgradeAPI{bound: 1, state: "active", workers: 2, versionStatus: map[int]int{1: http.StatusInternalServerError}}
	prior := clusterValues(verRef(1), verRef(1), 2, "active")
	prior["kubernetes_version"] = str(k8sAt(1))
	resp := refresh(t, api, prior)
	if len(resp.Diagnostics) > 0 {
		t.Fatalf("diagnostics: %v", resp.Diagnostics)
	}
	if got := stateValue(t, resp.State, "kubernetes_version"); got.ValueString() != k8sAt(1) {
		t.Errorf("kubernetes_version = %v", got)
	}
	if api.versionGETs != 0 {
		t.Errorf("%d version reads, want none", api.versionGETs)
	}
}

// A cluster type read that fails after a failed step mustn't cost the record
// of the step: the error still names the version the cluster is failed at,
// and state still records it, its Kubernetes version included.
func TestAFailedStepSurvivesAClusterTypeReadThatFails(t *testing.T) {
	api := &upgradeAPI{bound: 4, state: "active", workers: 2, failInto: 5, versionFailsOnceUpgrading: true}
	resp := runUpgrade(t, api, 5, 2)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a failed upgrade reported success")
	}
	e := resp.Diagnostics.Errors()[0]
	if !strings.Contains(e.Summary(), "failed at version 4") || strings.Contains(e.Summary()+e.Detail(), "did not settle") {
		t.Errorf("diagnostic:\n%s\n%s", e.Summary(), e.Detail())
	}
	for attr, want := range map[string]string{"state": "failed", "cluster_type_resolved": verRef(4), "kubernetes_version": k8sAt(4)} {
		if got := stateAttr(t, resp.State, attr); got != want {
			t.Errorf("%s = %q, want %q", attr, got, want)
		}
	}
}

// The data source reads the same version the resource does.
func TestTheClusterDataSourceReportsTheKubernetesVersion(t *testing.T) {
	server := (&upgradeAPI{bound: 4, state: "active", workers: 2}).serve(t)
	d := &clusterDataSource{meta: testClusterMeta(t, server.URL, time.Minute)}
	var s datasource.SchemaResponse
	d.Schema(context.Background(), datasource.SchemaRequest{}, &s)
	object, ok := s.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema is not an object")
	}
	values := map[string]tftypes.Value{}
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	values["id"] = str("c1")
	config := tftypes.NewValue(object, values)
	resp := datasource.ReadResponse{State: tfsdk.State{Raw: config, Schema: s.Schema}}
	d.Read(context.Background(), datasource.ReadRequest{Config: tfsdk.Config{Raw: config, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	var v types.String
	resp.State.GetAttribute(context.Background(), path.Root("kubernetes_version"), &v)
	if v.ValueString() != k8sAt(4) {
		t.Errorf("kubernetes_version = %v, want %q", v, k8sAt(4))
	}
}
