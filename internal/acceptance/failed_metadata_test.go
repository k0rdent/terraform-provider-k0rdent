package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"testing"
	"time"
)

// A network whose display name carries settlesFailed settles `failed` in the
// fake and stays there.
const settlesFailed = "fake-settles-failed"

// metadataNetwork is an evpn network with one tag. It has no `timeouts`
// block, so an apply waits as long as a default configuration does.
func metadataNetwork(name, displayName, team string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  display_name = %q
  tags         = { team = %q }
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }
}
`, networkType, name, displayName, team)
}

// metadataGroup is an instance group of one with a description. An instance
// group has no tags: its display name and description are what can change.
func metadataGroup(name, displayName, description string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = %q
  description  = %q
  node_pool = {
    "gpu-workers" = { size = 1 }
  }
}
`, instanceGroupType, name, clusterTypeRef, displayName, description)
}

// fakeObject reads path from the fake, past the provider.
func fakeObject(t *testing.T, path string) map[string]any {
	t.Helper()
	status, body, err := readFake(path)
	if err != nil || status != http.StatusOK {
		t.Fatalf("reading %s from the fake: %d %v", path, status, err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// teamIs says whether an object read from the fake has the tag team = want.
func teamIs(got map[string]any, want string) bool {
	tags, ok := got["tags"].(map[string]any)
	return ok && tags["team"] == want
}

// The platform takes a change to a `failed` network's tags, display name or
// description, or a `failed` instance group's display name or description,
// and leaves it `failed`. There's no operation to wait for, so the apply
// doesn't wait for it to become `active`. Each object here failed its create
// and was untainted, which keeps it in state.
func TestAccAMetadataChangeToAFailedNetworkOrInstanceGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const (
		networks = "/v1/regions/us-texas-1/projects/demo/networking/networks/"
		groups   = "/v1/regions/us-texas-1/projects/demo/compute/instance-groups/"
	)
	for _, tc := range []struct {
		name, typeName, path string
		// before fails its create, and after changes only metadata.
		before, after string
		// holds says whether the fake holds what after sent.
		holds func(got map[string]any) bool
	}{
		{
			name: "acc-failed-network-tags", typeName: networkType, path: networks,
			before: metadataNetwork("acc-failed-network-tags", "acceptance "+settlesFailed, "ml"),
			after:  metadataNetwork("acc-failed-network-tags", "acceptance "+settlesFailed, "infra"),
			holds:  func(got map[string]any) bool { return teamIs(got, "infra") },
		},
		{
			name: "acc-failed-network-name", typeName: networkType, path: networks,
			before: metadataNetwork("acc-failed-network-name", "acceptance "+settlesFailed, "ml"),
			after:  metadataNetwork("acc-failed-network-name", "renamed "+settlesFailed, "ml"),
			holds:  func(got map[string]any) bool { return got["displayName"] == "renamed "+settlesFailed },
		},
		{
			name: "acc-failed-group-description", typeName: instanceGroupType, path: groups,
			before: metadataGroup("acc-failed-group-description", "acceptance "+createFails, "before"),
			after:  metadataGroup("acc-failed-group-description", "acceptance "+createFails, "after"),
			holds:  func(got map[string]any) bool { return got["description"] == "after" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := tc.typeName + ".test"
			path := tc.path + tc.name
			c := newCLIRun(t)
			c.write(tc.before)
			if out, ok := c.run("apply", "-auto-approve"); ok {
				t.Fatalf("the create passed, want it failed:\n%s", out)
			}
			t.Cleanup(func() {
				if out, ok := c.run("destroy", "-auto-approve"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})
			if out, ok := c.run("untaint", addr); !ok {
				t.Fatalf("untaint:\n%s", out)
			}
			awaitFakeState(t, path, "failed")

			c.write(tc.after)
			start := time.Now()
			out, ok := c.run("apply", "-auto-approve")
			if !ok {
				t.Fatalf("the metadata change to a failed %s failed:\n%s", tc.typeName, out)
			}
			if took := time.Since(start); took > 30*time.Second {
				t.Errorf("the metadata change to a failed %s took %s", tc.typeName, took.Round(time.Second))
			}
			mustSay(t, "the metadata change", out, "0 added, 1 changed, 0 destroyed")
			got := fakeObject(t, path)
			if !tc.holds(got) {
				t.Errorf("the fake doesn't hold the change: %v", got)
			}
			if got["state"] != "failed" {
				t.Errorf("the fake has the %s %v, want failed", tc.typeName, got["state"])
			}
			out, _ = c.run("plan")
			mustSay(t, "the plan after the metadata change", out, "No changes.")

			out, ok = c.run("destroy", "-auto-approve")
			if !ok {
				t.Fatalf("the destroy of the failed %s failed:\n%s", tc.typeName, out)
			}
			mustSay(t, "the destroy", out, "1 destroyed")
		})
	}
}

// A metadata change to an `active` network or instance group still reaches
// the platform and plans clean after.
func TestAccAMetadataChangeToAnActiveNetworkOrInstanceGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		name, path    string
		before, after string
		holds         func(got map[string]any) bool
	}{
		{
			name:   "acc-active-network-tags",
			path:   "/v1/regions/us-texas-1/projects/demo/networking/networks/acc-active-network-tags",
			before: metadataNetwork("acc-active-network-tags", "acceptance", "ml"),
			after:  metadataNetwork("acc-active-network-tags", "acceptance", "infra"),
			holds:  func(got map[string]any) bool { return teamIs(got, "infra") },
		},
		{
			name:   "acc-active-group-description",
			path:   "/v1/regions/us-texas-1/projects/demo/compute/instance-groups/acc-active-group-description",
			before: metadataGroup("acc-active-group-description", "acceptance", "before"),
			after:  metadataGroup("acc-active-group-description", "acceptance", "after"),
			holds:  func(got map[string]any) bool { return got["description"] == "after" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLIRun(t)
			c.write(tc.before)
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("create:\n%s", out)
			}
			t.Cleanup(func() {
				if out, ok := c.run("destroy", "-auto-approve"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})
			c.write(tc.after)
			out, ok := c.run("apply", "-auto-approve")
			if !ok {
				t.Fatalf("the metadata change failed:\n%s", out)
			}
			mustSay(t, "the metadata change", out, "0 added, 1 changed, 0 destroyed")
			got := fakeObject(t, tc.path)
			if !tc.holds(got) {
				t.Errorf("the fake doesn't hold the change: %v", got)
			}
			if got["state"] != "active" {
				t.Errorf("the fake has it %v, want active", got["state"])
			}
			out, _ = c.run("plan")
			mustSay(t, "the plan after the metadata change", out, "No changes.")
		})
	}
}
