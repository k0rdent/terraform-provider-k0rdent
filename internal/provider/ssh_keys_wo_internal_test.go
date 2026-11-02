package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// Two keys and the fingerprints `ssh-keygen -lf` prints for them. The
// platform reports a credential's fingerprint in the same form.
const (
	keyA   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKVuRV8uUSyy9yMMkFxPDXfVDWycctdRciRFs2VFIP8Z probe@kcs2558"
	keyAFP = "SHA256:nntdsoIbh6SRm4EuGk+4hKWmGnkFW9ekoukDl9kO7Po"
	keyB   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPa4wYqS47mN2185oA1abzR+PC9eTG2mlm08ZEKFsOLr other"
	keyBFP = "SHA256:jMvxSQ/V5s0Erqv1lMbii+hI79DZUtlu8lNffovVENA"
)

func TestAKeyFingerprintIsTheOneSSHKeygenPrints(t *testing.T) {
	for key, want := range map[string]string{keyA: keyAFP, keyB: keyBFP, "not a key": ""} {
		if got := sshFingerprint(key); got != want {
			t.Errorf("sshFingerprint(%q) = %q, want %q", key, got, want)
		}
	}
}

// The create sends a user's write-only keys, and a user given only
// references sends those: the platform takes one or the other.
func TestACreateSendsTheWriteOnlyKeysOrTheReferences(t *testing.T) {
	ctx := context.Background()
	ref := "/v1/regions/local/projects/p/credentials/mcred-0123456789abcdefghjkmnpqrs"
	users := []sshUserModel{
		{Username: types.StringValue("ops"), SSHPublicKeys: types.ListNull(types.StringType),
			Credentials: types.ListNull(types.StringType)},
		{Username: types.StringValue("dev"), SSHPublicKeys: types.ListNull(types.StringType),
			Credentials: types.ListValueMust(types.StringType, []attr.Value{types.StringValue(ref)})},
	}
	set, d := types.SetValueFrom(ctx, sshUserObjectType(), users)
	if d.HasError() {
		t.Fatal(d)
	}
	got, d := sshAccessRequest(ctx, set, map[string][]string{"ops": {keyA}})
	if d.HasError() {
		t.Fatal(d)
	}
	by := map[string]k0rdent.SshUser{}
	for _, u := range *got {
		by[u.Username] = u
	}
	if ops := by["ops"]; ops.SshPublicKeys == nil || !slices.Equal(*ops.SshPublicKeys, []string{keyA}) || ops.SshPublicKeyCredentials != nil {
		t.Errorf("ops sent %+v, want its write-only key and no references", ops)
	}
	if dev := by["dev"]; dev.SshPublicKeyCredentials == nil || !slices.Equal(*dev.SshPublicKeyCredentials, []string{ref}) || dev.SshPublicKeys != nil {
		t.Errorf("dev sent %+v, want its reference and no keys", dev)
	}
}

// What an import reads: the platform's references, which is what makes a
// plan with the same configuration clean.
func TestAReadTakesTheReferencesThePlatformReports(t *testing.T) {
	ctx := context.Background()
	ref := "/v1/regions/local/projects/p/credentials/mcred-0123456789abcdefghjkmnpqrs"
	got, d := sshAccessValue(ctx, &[]k0rdent.SshUser{{Username: "ops", SshPublicKeyCredentials: &[]string{ref}}})
	if d.HasError() {
		t.Fatal(d)
	}
	var users []sshUserModel
	if d := got.ElementsAs(ctx, &users, false); d.HasError() {
		t.Fatal(d)
	}
	var refs []string
	if len(users) != 1 || users[0].Credentials.ElementsAs(ctx, &refs, false).HasError() || !slices.Equal(refs, []string{ref}) {
		t.Errorf("read %v, want ops with reference %s", users, ref)
	}
	if !users[0].SSHPublicKeys.IsNull() {
		t.Errorf("keys = %v, want null: the platform doesn't report them", users[0].SSHPublicKeys)
	}
}

// A changed key replaces. The same key, a user the provider holds nothing
// for (an import), or no keys configured don't.
func TestAWriteOnlyKeyChangeIsFoundByFingerprint(t *testing.T) {
	held := map[string][]string{"ops": {keyAFP}}
	for name, tt := range map[string]struct {
		held, want map[string][]string
		changed    bool
	}{
		"same key":        {held, fingerprintsOf(map[string][]string{"ops": {keyA}}), false},
		"changed key":     {held, fingerprintsOf(map[string][]string{"ops": {keyB}}), true},
		"a key added":     {held, fingerprintsOf(map[string][]string{"ops": {keyA, keyB}}), true},
		"after an import": {nil, fingerprintsOf(map[string][]string{"ops": {keyB}}), false},
		"no keys written": {held, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := len(keysChanged(tt.held, tt.want)) > 0; got != tt.changed {
				t.Errorf("keysChanged = %v, want %v", got, tt.changed)
			}
		})
	}
}

// Keys that only move, between `ssh_public_keys` and `ssh_public_keys_wo` or
// in over none after an import, change nothing on the nodes and replace
// nothing. A different key, by fingerprint, or a changed reference replaces.
// Keys written over none with no fingerprints to check them against, and the
// references of a user with none, which only planUncheckedKeys plans unknown,
// are left to replaceOnChangedKeys, which plans the update that stops at the
// apply.
func TestKeysThatOnlyMoveReplaceNothing(t *testing.T) {
	ctx := context.Background()
	ref := "/v1/regions/local/projects/p/credentials/mcred-0123456789abcdefghjkmnpqrs"
	other := "/v1/regions/local/projects/p/credentials/mcred-0123456789abcdefghjkmnpqrt"
	user := func(keys []string, refs ...string) types.Set {
		u := sshUserModel{Username: types.StringValue("ops"), SSHPublicKeys: types.ListNull(types.StringType),
			Credentials: types.ListNull(types.StringType), Sudo: types.StringNull(), DisplayName: types.StringNull()}
		if keys != nil {
			u.SSHPublicKeys, _ = types.ListValueFrom(ctx, types.StringType, keys)
		}
		if refs != nil {
			u.Credentials, _ = types.ListValueFrom(ctx, types.StringType, refs)
		}
		set, d := types.SetValueFrom(ctx, sshUserObjectType(), []sshUserModel{u})
		if d.HasError() {
			t.Fatal(d)
		}
		return set
	}
	madeWithA := map[string][]string{"ops": {keyAFP}}
	woA, woB := map[string][]string{"ops": {keyA}}, map[string][]string{"ops": {keyB}}
	unknownRefs, d := types.SetValueFrom(ctx, sshUserObjectType(), []sshUserModel{{Username: types.StringValue("ops"),
		SSHPublicKeys: types.ListNull(types.StringType), Credentials: types.ListUnknown(types.StringType),
		Sudo: types.StringNull(), DisplayName: types.StringNull()}})
	if d.HasError() {
		t.Fatal(d)
	}
	refs, _ := types.ListValueFrom(ctx, types.StringType, []string{ref})
	unknownText, d := types.SetValueFrom(ctx, sshUserObjectType(), []sshUserModel{{Username: types.StringValue("ops"),
		SSHPublicKeys: types.ListUnknown(types.StringType), Credentials: refs,
		Sudo: types.StringNull(), DisplayName: types.StringNull()}})
	if d.HasError() {
		t.Fatal(d)
	}
	for name, tt := range map[string]struct {
		state, plan         types.Set
		writeOnly, madeWith map[string][]string
		moved, _            bool
	}{
		"imported, keys written, none read":    {user(nil, ref), user([]string{keyA}, ref), nil, nil, true, true},
		"imported, written as before":          {user([]string{}, ref), user([]string{keyA}, ref), nil, nil, true, true},
		"imported, the platform's key written": {user(nil, ref), user([]string{keyA}, ref), nil, madeWithA, true, false},
		"imported, another key written":        {user(nil, ref), user([]string{keyB}, ref), nil, madeWithA, false, false},
		"imported, the keys in another order": {user(nil, ref, other), user([]string{keyB, keyA}, ref, other), nil,
			map[string][]string{"ops": {keyAFP, keyBFP}}, true, false},
		"made with A, A written":               {user(nil, ref), user([]string{keyA}, ref), nil, madeWithA, true, false},
		"made with A, B written":               {user(nil, ref), user([]string{keyB}, ref), nil, madeWithA, false, false},
		"moved to write-only, same key":        {user([]string{keyA}, ref), user(nil, ref), woA, nil, true, false},
		"moved to write-only, other key":       {user([]string{keyA}, ref), user(nil, ref), woB, nil, false, false},
		"moved to a key not yet known":         {user([]string{keyA}, ref), user(nil, ref), map[string][]string{"ops": nil}, nil, false, false},
		"moved from write-only, same key":      {user(nil, ref), user([]string{keyA}, ref), nil, madeWithA, true, false},
		"keys left out":                        {user([]string{keyA}, ref), user(nil, ref), nil, nil, true, false},
		"reference changed":                    {user(nil, ref), user(nil, other), nil, nil, false, false},
		"reference changed with the same keys": {user([]string{keyA}, ref), user([]string{keyA}, other), nil, nil, false, false},
		"references not known until the apply": {user(nil, ref), unknownRefs, nil, madeWithA, false, false},
		"references unknown with none held":    {user(nil, ref), unknownRefs, nil, nil, true, false},
		"text keys not known until the apply":  {user([]string{keyA}, ref), unknownText, nil, madeWithA, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			var diags diag.Diagnostics
			if got := keysMoved(ctx, tt.state, tt.plan, tt.writeOnly, tt.madeWith, &diags); got != tt.moved {
				t.Errorf("moved = %v, want %v", got, tt.moved)
			}
			if len(diags) != 0 {
				t.Errorf("diagnostics: %v", diags)
			}
		})
	}
}

// A group whose write-only keys aren't known at plan can't show they only
// moved, so a change to its users replaces it.
func TestAGroupWriteOnlyKeyUnknownAtPlanReplaces(t *testing.T) {
	ctx := context.Background()
	unknownKey := tftypes.NewValue(tftypes.Map{ElementType: tftypes.List{ElementType: tftypes.String}}, tftypes.UnknownValue)
	configRaw, s := testValue(t, &instanceGroupResource{}, with(opsGroup("active"),
		map[string]tftypes.Value{"ssh_access": woUsers(), "ssh_public_keys_wo": unknownKey}))
	access, d := types.SetValueFrom(ctx, sshUserObjectType(), []sshUserModel{{Username: types.StringValue("ops"),
		SSHPublicKeys: types.ListNull(types.StringType), Credentials: types.ListNull(types.StringType),
		Sudo: types.StringNull(), DisplayName: types.StringNull()}})
	if d.HasError() {
		t.Fatal(d)
	}
	req := planmodifier.SetRequest{Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
		ConfigValue: access, StateValue: access, PlanValue: access}
	freshPrivate(t, &req.Private, nil)
	var resp setplanmodifier.RequiresReplaceIfFuncResponse
	sshAccessRequiresReplace(ctx, req, &resp)
	if !resp.RequiresReplace {
		t.Error("a write-only key unknown at plan doesn't replace the group")
	}
}

// woUsers is `ssh_access` with ops and no keys, as a configuration that
// writes them in `ssh_public_keys_wo` has it, and woKeys is that map.
func woUsers() tftypes.Value {
	return tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{sshUserWith("ops", nil)})
}

// sshUserWith is a user with the given keys, null for none.
func sshUserWith(name string, keys []tftypes.Value) tftypes.Value {
	list := tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, nil)
	if keys != nil {
		list = tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, keys)
	}
	return tftypes.NewValue(sshUserTF, map[string]tftypes.Value{
		"username":            str(name),
		"ssh_public_keys":     list,
		"ssh_public_key_refs": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, nil),
		"sudo":                tftypes.NewValue(tftypes.String, nil),
		"display_name":        tftypes.NewValue(tftypes.String, nil),
	})
}

func woKeys(keys ...string) tftypes.Value {
	vals := make([]tftypes.Value, 0, len(keys))
	for _, k := range keys {
		vals = append(vals, str(k))
	}
	list := tftypes.List{ElementType: tftypes.String}
	return tftypes.NewValue(tftypes.Map{ElementType: list}, map[string]tftypes.Value{"ops": tftypes.NewValue(list, vals)})
}

// A cluster create sends the write-only keys and holds their fingerprints,
// which a later plan checks the keys against. State takes the references the
// platform made from the keys, read back after the create.
func TestAClusterCreateHoldsTheFingerprintsOfTheKeysItSent(t *testing.T) {
	const ref = "/v1/regions/test/projects/test/credentials/mcred-01jabcdefghjkmnpqrstvwxyz0"
	var g groupsServer
	cluster, err := url.Parse(g.start(t).URL)
	if err != nil {
		t.Fatal(err)
	}
	rest := httputil.NewSingleHostReverseProxy(cluster)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ssh-access") {
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `[{"username":"ops","sshPublicKeyCredentials":["`+ref+`"]}]`); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		rest.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	planRaw, s := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": woUsers()}))
	configRaw, _ := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": woUsers(), "ssh_public_keys_wo": woKeys(keyA)}))
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
	freshPrivate(t, &resp.Private, nil)
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
		Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics.Errors())
	}
	sent := g.bodies()
	if len(sent) != 1 || !reflect.DeepEqual(sent[0]["sshAccess"], []any{map[string]any{"username": "ops", "sshPublicKeys": []any{keyA}}}) {
		t.Errorf("sent %v, want ops with the write-only key", sent)
	}
	held := loadFingerprints(context.Background(), privateIn(t, resp.Private))
	if !reflect.DeepEqual(held, map[string][]string{"ops": {keyAFP}}) {
		t.Errorf("held %v, want the key's fingerprint", held)
	}
	var stored types.Map
	resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("ssh_public_keys_wo"), &stored)...)
	if !stored.IsNull() {
		t.Errorf("state holds ssh_public_keys_wo = %v, want null", stored)
	}
	var access types.Set
	var users []sshUserModel
	var refs []string
	resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("ssh_access"), &access)...)
	resp.Diagnostics.Append(access.ElementsAs(context.Background(), &users, false)...)
	if len(users) == 1 {
		resp.Diagnostics.Append(users[0].Credentials.ElementsAs(context.Background(), &refs, false)...)
	}
	if resp.Diagnostics.HasError() || !slices.Equal(refs, []string{ref}) {
		t.Errorf("state ssh_access = %v (%v), want ops with the reference the platform made", access, resp.Diagnostics)
	}
}

// An instance group's create holds the fingerprints too.
func TestAGroupCreateHoldsTheFingerprintsOfTheKeysItSent(t *testing.T) {
	body := deletingGroupBody("active")
	delete(body, "sshAccess")
	platform := &pendingPlatform{body: body}
	r := &instanceGroupResource{meta: testClusterMeta(t, platform.serve(t).URL, time.Minute)}
	// References planned unknown, as a real plan has them, and an answer that
	// leaves them out: the apply can't end with them still unknown.
	unknownRefs := tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{tftypes.NewValue(sshUserTF, map[string]tftypes.Value{
		"username":            str("ops"),
		"ssh_public_keys":     tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, nil),
		"ssh_public_key_refs": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, tftypes.UnknownValue),
		"sudo":                tftypes.NewValue(tftypes.String, nil),
		"display_name":        tftypes.NewValue(tftypes.String, nil),
	})})
	planned := with(opsGroup("active"), map[string]tftypes.Value{"ssh_access": unknownRefs,
		"state": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})
	planRaw, s := testValue(t, r, planned)
	configRaw, _ := testValue(t, r, with(planned, map[string]tftypes.Value{"ssh_public_keys_wo": woKeys(keyA)}))
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
	freshPrivate(t, &resp.Private, nil)
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
		Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics.Errors())
	}
	held := loadFingerprints(context.Background(), privateIn(t, resp.Private))
	if !reflect.DeepEqual(held, map[string][]string{"ops": {keyAFP}}) {
		t.Errorf("held %v, want the key's fingerprint", held)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Errorf("state holds unknown values after the create: %v", resp.State.Raw)
	}
}

// A plan whose write-only key differs, by fingerprint, from the one the nodes
// were made with is refused as a replacement, on both resources, and the
// refusal says a key differs: `ssh_access` itself didn't move. The same
// key plans nothing.
func TestAChangedWriteOnlyKeyIsRefusedAsAReplacement(t *testing.T) {
	for _, rc := range []struct {
		name string
		r    guardable
		what tenantState
		base map[string]tftypes.Value
	}{
		{"cluster", &clusterResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}, clusterTenantState, existingCluster(nil)},
		{"instance group", &instanceGroupResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}, instanceGroupTenantState, opsGroup("active")},
	} {
		for key, refused := range map[string]bool{keyA: false, keyB: true} {
			t.Run(fmt.Sprintf("%s/refused %v", rc.name, refused), func(t *testing.T) {
				r := guardedModifier(t, rc.r, rc.what)
				stateRaw, s := testValue(t, r, with(rc.base, map[string]tftypes.Value{"ssh_access": woUsers()}))
				configRaw, _ := testValue(t, r, with(rc.base, map[string]tftypes.Value{"ssh_access": woUsers(), "ssh_public_keys_wo": woKeys(key)}))
				plan := tfsdk.Plan{Raw: stateRaw, Schema: s.Schema}
				req := resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema}, Plan: plan,
					State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
				freshPrivate(t, &req.Private, nil)
				if d := saveFingerprints(context.Background(), privateIn(t, req.Private), map[string][]string{"ops": {keyAFP}}); d.HasError() {
					t.Fatal(d)
				}
				resp := resource.ModifyPlanResponse{Plan: plan}
				r.ModifyPlan(context.Background(), req, &resp)
				errs := resp.Diagnostics.Errors()
				if got := len(errs) == 1 && strings.Contains(errs[0].Summary(), "would replace"); got != refused {
					t.Fatalf("refused = %v, want %v: %v", got, refused, resp.Diagnostics)
				}
				if refused && !strings.Contains(errs[0].Detail(), "A configured SSH key, or a key reference, differs") {
					t.Errorf("the refusal doesn't say a key differs:\n%s", errs[0].Detail())
				}
			})
		}
	}
}

// A create fails at plan on a user with no keys anywhere, which the platform
// refuses, on keys written both ways, and on write-only keys for a user
// `ssh_access` doesn't name, which would go nowhere. On an existing cluster the
// last two warn, so a destroy's plan still runs, and leaving keys out is what
// an import does.
func TestKeysTheCreateCantSendFailThePlan(t *testing.T) {
	r := &clusterResource{}
	eve := tftypes.NewValue(tftypes.Map{ElementType: tftypes.List{ElementType: tftypes.String}}, map[string]tftypes.Value{
		"eve": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str(keyA)}),
	})
	unknownKey := tftypes.NewValue(tftypes.Map{ElementType: tftypes.List{ElementType: tftypes.String}}, map[string]tftypes.Value{
		"ops": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}),
	})
	withRefs := tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{tftypes.NewValue(sshUserTF, map[string]tftypes.Value{
		"username":            str("ops"),
		"ssh_public_keys":     tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, nil),
		"ssh_public_key_refs": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str("/v1/regions/test/projects/test/credentials/mcred-01jabcdefghjkmnpqrstvwxyz0")}),
		"sudo":                tftypes.NewValue(tftypes.String, nil),
		"display_name":        tftypes.NewValue(tftypes.String, nil),
	})})
	unknownRefsAccess := tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{tftypes.NewValue(sshUserTF, map[string]tftypes.Value{
		"username":            str("ops"),
		"ssh_public_keys":     tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, nil),
		"ssh_public_key_refs": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, tftypes.UnknownValue),
		"sudo":                tftypes.NewValue(tftypes.String, nil),
		"display_name":        tftypes.NewValue(tftypes.String, nil),
	})})
	twice := tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{
		sshUser("ops", tftypes.NewValue(tftypes.String, nil), "ssh-ed25519 AAAA ops"),
		sshUser("ops", str("ALL=(ALL) ALL"), "ssh-ed25519 AAAA ops"),
	})
	for name, tt := range map[string]struct {
		config   map[string]tftypes.Value
		existing bool
		want     string
		warning  bool
	}{
		"keys written in ssh_public_keys_wo":         {map[string]tftypes.Value{"ssh_access": woUsers(), "ssh_public_keys_wo": woKeys(keyA)}, false, "", false},
		"a write-only key not known until the apply": {map[string]tftypes.Value{"ssh_access": woUsers(), "ssh_public_keys_wo": unknownKey}, false, "", false},
		"keys written in ssh_public_keys":            {map[string]tftypes.Value{"ssh_access": opsAccess()}, false, "", false},
		"no keys on a create":                        {map[string]tftypes.Value{"ssh_access": woUsers()}, false, "with no keys", false},
		"no keys on an existing cluster":             {map[string]tftypes.Value{"ssh_access": woUsers()}, true, "", false},
		"keys written both ways on a create":         {map[string]tftypes.Value{"ssh_access": opsAccess(), "ssh_public_keys_wo": woKeys(keyA)}, false, "in both", false},
		"keys for a user ssh_access lacks":           {map[string]tftypes.Value{"ssh_access": opsAccess(), "ssh_public_keys_wo": eve}, false, "no user of that name", false},
		"keys for a user lacking, existing cluster":  {map[string]tftypes.Value{"ssh_access": opsAccess(), "ssh_public_keys_wo": eve}, true, "no user of that name", true},
		"keys and references on a create":            {map[string]tftypes.Value{"ssh_access": withRefs, "ssh_public_keys_wo": woKeys(keyA)}, false, "and `ssh_public_key_refs`", false},
		"keys and references not known yet":          {map[string]tftypes.Value{"ssh_access": unknownRefsAccess, "ssh_public_keys_wo": woKeys(keyA)}, false, "and `ssh_public_key_refs`", false},
		"a username twice":                           {map[string]tftypes.Value{"ssh_access": twice}, false, "more than once", false},
		"no users and no keys":                       {nil, false, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			raw, s := testValue(t, r, existingCluster(tt.config))
			req := resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: raw, Schema: s.Schema},
				Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}, State: tfsdk.State{Raw: tftypes.NewValue(raw.Type(), nil), Schema: s.Schema}}
			if tt.existing {
				req.State.Raw = raw
			}
			var diags diag.Diagnostics
			checkConfiguredKeys(context.Background(), req, &diags)
			got, warned := "", false
			switch {
			case diags.HasError():
				got = diags.Errors()[0].Detail()
			case diags.WarningsCount() > 0:
				got, warned = diags.Warnings()[0].Detail(), true
			}
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) || warned != tt.warning {
				t.Errorf("got %q (warning %v), want one containing %q (warning %v)", got, warned, tt.want, tt.warning)
			}
		})
	}
}

// Both resources run the check on a create's plan.
func TestBothResourcesCheckTheKeysOnACreate(t *testing.T) {
	for name, tt := range map[string]struct {
		r    resource.ResourceWithModifyPlan
		base map[string]tftypes.Value
	}{
		"cluster":        {&clusterResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}, existingCluster(nil)},
		"instance group": {&instanceGroupResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}, opsGroup("active")},
	} {
		t.Run(name, func(t *testing.T) {
			raw, s := testValue(t, tt.r, with(tt.base, map[string]tftypes.Value{"ssh_access": woUsers()}))
			plan := tfsdk.Plan{Raw: raw, Schema: s.Schema}
			resp := resource.ModifyPlanResponse{Plan: plan}
			tt.r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: raw, Schema: s.Schema},
				Plan: plan, State: tfsdk.State{Raw: tftypes.NewValue(raw.Type(), nil), Schema: s.Schema}}, &resp)
			if errs := resp.Diagnostics.Errors(); len(errs) != 1 || !strings.Contains(errs[0].Summary(), "has no keys") {
				t.Errorf("errors = %v, want the user without keys", errs)
			}
		})
	}
}

// A create that leaves `ssh_access` out plans no users, known at plan, on
// both resources: the create sends none and the platform reports none.
func TestACreateWithoutSSHAccessPlansNoUsers(t *testing.T) {
	for name, tt := range map[string]struct {
		r    resource.ResourceWithModifyPlan
		base map[string]tftypes.Value
	}{
		"cluster":        {&clusterResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}, existingCluster(nil)},
		"instance group": {&instanceGroupResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}, opsGroup("active")},
	} {
		t.Run(name, func(t *testing.T) {
			configRaw, s := testValue(t, tt.r, with(tt.base, map[string]tftypes.Value{"ssh_access": tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, nil)}))
			planRaw, _ := testValue(t, tt.r, with(tt.base, map[string]tftypes.Value{"ssh_access": tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, tftypes.UnknownValue)}))
			plan := tfsdk.Plan{Raw: planRaw, Schema: s.Schema}
			resp := resource.ModifyPlanResponse{Plan: plan}
			tt.r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
				Plan: plan, State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}, &resp)
			var planned types.Set
			resp.Diagnostics.Append(resp.Plan.GetAttribute(context.Background(), path.Root("ssh_access"), &planned)...)
			if resp.Diagnostics.HasError() || !planned.IsNull() {
				t.Errorf("ssh_access planned %v (%v), want null", planned, resp.Diagnostics)
			}
		})
	}
}

// A plan that differs from state only in attributes the configuration leaves
// out, which the framework marked unknown, is no change. One that changes
// anything the configuration sets, or leaves unknown an attribute it sets, is
// left alone.
func TestAPlanThatOnlyMarkedLeftOutAttributesUnknownIsNoChange(t *testing.T) {
	r := &instanceGroupResource{}
	stateRaw, s := testValue(t, r, opsGroup("failed"))
	unknownState := with(opsGroup("failed"), map[string]tftypes.Value{"state": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})
	for name, tt := range map[string]struct {
		plan, config map[string]tftypes.Value
		noChange     bool
	}{
		"only state unknown": {unknownState, with(opsGroup("failed"), map[string]tftypes.Value{"state": tftypes.NewValue(tftypes.String, nil)}), true},
		"a renamed group": {with(unknownState, map[string]tftypes.Value{"display_name": str("renamed")}),
			with(opsGroup("failed"), map[string]tftypes.Value{"state": tftypes.NewValue(tftypes.String, nil), "display_name": str("renamed")}), false},
		"a left-out value planned anew": {with(opsGroup("failed"), map[string]tftypes.Value{"state": str("active")}),
			with(opsGroup("failed"), map[string]tftypes.Value{"state": tftypes.NewValue(tftypes.String, nil)}), false},
		"a configured value unknown": {with(unknownState, map[string]tftypes.Value{"display_name": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}),
			with(opsGroup("failed"), map[string]tftypes.Value{"state": tftypes.NewValue(tftypes.String, nil), "display_name": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}), false},
	} {
		t.Run(name, func(t *testing.T) {
			planRaw, _ := testValue(t, r, tt.plan)
			configRaw, _ := testValue(t, r, tt.config)
			plan := tfsdk.Plan{Raw: planRaw, Schema: s.Schema}
			resp := resource.ModifyPlanResponse{Plan: plan}
			planNoChangeOverUnreadAttributes(resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
				Plan: plan, State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}, &resp)
			if got := resp.Plan.Raw.Equal(stateRaw); got != tt.noChange {
				t.Errorf("no change = %v, want %v", got, tt.noChange)
			}
		})
	}
}

// Both resources plan a group or cluster left `failed`, with `ssh_access`
// left out, as no change: a cluster with `on_failed = "keep"`, which asks for
// that.
func TestAFailedResourceWithSSHAccessLeftOutPlansNoChange(t *testing.T) {
	for name, tt := range map[string]struct {
		r    resource.ResourceWithModifyPlan
		base map[string]tftypes.Value
	}{
		"cluster": {&clusterResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}, with(existingCluster(nil), map[string]tftypes.Value{"state": str("failed"), "on_failed": str("keep"),
			"tags_all": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{})})},
		"instance group": {&instanceGroupResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}, opsGroup("failed")},
	} {
		t.Run(name, func(t *testing.T) {
			stateRaw, s := testValue(t, tt.r, with(tt.base, map[string]tftypes.Value{"ssh_access": opsAccess()}))
			planRaw, _ := testValue(t, tt.r, with(tt.base, map[string]tftypes.Value{"ssh_access": opsAccess(),
				"state": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}))
			configRaw, _ := testValue(t, tt.r, with(tt.base, map[string]tftypes.Value{"state": tftypes.NewValue(tftypes.String, nil)}))
			plan := tfsdk.Plan{Raw: planRaw, Schema: s.Schema}
			resp := resource.ModifyPlanResponse{Plan: plan}
			tt.r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
				Plan: plan, State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}, &resp)
			if !resp.Plan.Raw.Equal(stateRaw) {
				diffs, err := stateRaw.Diff(resp.Plan.Raw)
				t.Errorf("planned a change: %v %v (%v)", diffs, err, resp.Diagnostics)
			}
		})
	}
}

// A changed write-only key plans the user's references unknown, so that
// `ssh_access` itself changes and Terraform carries out the replacement
// rather than dropping it. The same key plans nothing.
func TestAChangedWriteOnlyKeyPlansNewReferences(t *testing.T) {
	ref := "/v1/regions/test/projects/test/credentials/mcred-01jabcdefghjkmnpqrstvwxyz0"
	withRef := tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{tftypes.NewValue(sshUserTF, map[string]tftypes.Value{
		"username":            str("ops"),
		"ssh_public_keys":     tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, nil),
		"ssh_public_key_refs": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str(ref)}),
		"sudo":                tftypes.NewValue(tftypes.String, nil),
		"display_name":        tftypes.NewValue(tftypes.String, nil),
	})})
	r := &clusterResource{}
	for key, replaced := range map[string]bool{keyA: false, keyB: true} {
		stateRaw, s := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": withRef}))
		configRaw, _ := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": woUsers(), "ssh_public_keys_wo": woKeys(key)}))
		plan := tfsdk.Plan{Raw: stateRaw, Schema: s.Schema}
		req := resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema}, Plan: plan,
			State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
		freshPrivate(t, &req.Private, nil)
		if d := saveFingerprints(context.Background(), privateIn(t, req.Private), map[string][]string{"ops": {keyAFP}}); d.HasError() {
			t.Fatal(d)
		}
		resp := resource.ModifyPlanResponse{Plan: plan}
		replaceOnChangedKeys(context.Background(), req, &resp)
		var planned types.Set
		var users []sshUserModel
		resp.Diagnostics.Append(resp.Plan.GetAttribute(context.Background(), path.Root("ssh_access"), &planned)...)
		resp.Diagnostics.Append(planned.ElementsAs(context.Background(), &users, false)...)
		if resp.Diagnostics.HasError() || len(users) != 1 {
			t.Fatalf("ssh_access = %v: %v", planned, resp.Diagnostics)
		}
		if got := users[0].Credentials.IsUnknown(); got != replaced || (len(resp.RequiresReplace) == 1) != replaced {
			t.Errorf("key %s: references unknown = %v and replaces %v, want %v", key[len(key)-6:], got, resp.RequiresReplace, replaced)
		}
	}
}

// Each user keeps its own references when keys move, whatever order the set
// is in. UseStateForUnknown matched them by position.
func TestReferencesStayWithTheirUser(t *testing.T) {
	ctx := context.Background()
	user := func(name string, refs types.List) sshUserModel {
		return sshUserModel{Username: types.StringValue(name), SSHPublicKeys: types.ListNull(types.StringType),
			Credentials: refs, Sudo: types.StringNull(), DisplayName: types.StringNull()}
	}
	refs := func(r string) types.List {
		return types.ListValueMust(types.StringType, []attr.Value{types.StringValue(r)})
	}
	set := func(users ...sshUserModel) types.Set {
		v, d := types.SetValueFrom(ctx, sshUserObjectType(), users)
		if d.HasError() {
			t.Fatal(d)
		}
		return v
	}
	state := set(user("bob", refs("rb")), user("alice", refs("ra")))
	for _, order := range [][]string{{"alice", "bob"}, {"bob", "alice"}} {
		plan := set(user(order[0], types.ListUnknown(types.StringType)), user(order[1], types.ListUnknown(types.StringType)))
		config := set(user(order[0], types.ListNull(types.StringType)), user(order[1], types.ListNull(types.StringType)))
		resp := planmodifier.SetResponse{PlanValue: plan}
		refsByUsername{}.PlanModifySet(ctx, planmodifier.SetRequest{StateValue: state, PlanValue: plan, ConfigValue: config}, &resp)
		var got []sshUserModel
		resp.Diagnostics.Append(resp.PlanValue.ElementsAs(ctx, &got, false)...)
		for _, u := range got {
			var r []string
			resp.Diagnostics.Append(u.Credentials.ElementsAs(ctx, &r, false)...)
			if want := "r" + u.Username.ValueString()[:1]; len(r) != 1 || r[0] != want {
				t.Errorf("order %v: %s planned %v, want [%s]", order, u.Username.ValueString(), r, want)
			}
		}
	}
}

// Keys state holds as text, from an earlier version's create or an import's
// recorded keys, are held as fingerprints for a later move to
// `ssh_public_keys_wo`. Fingerprints already held stay.
func TestKeysInStateAreHeldForALaterMove(t *testing.T) {
	ctx := context.Background()
	inline, d := types.SetValueFrom(ctx, sshUserObjectType(), []sshUserModel{{Username: types.StringValue("ops"),
		SSHPublicKeys: types.ListValueMust(types.StringType, []attr.Value{types.StringValue(keyA)}),
		Credentials:   types.ListNull(types.StringType), Sudo: types.StringNull(), DisplayName: types.StringNull()}})
	if d.HasError() {
		t.Fatal(d)
	}
	for name, tt := range map[string]struct {
		held, want map[string][]string
	}{
		"none held":    {nil, map[string][]string{"ops": {keyAFP}}},
		"already held": {map[string][]string{"ops": {keyBFP}}, map[string][]string{"ops": {keyBFP}}},
	} {
		t.Run(name, func(t *testing.T) {
			private := memPrivate{}
			if tt.held != nil {
				saveFingerprints(ctx, private, tt.held)
			}
			if d := holdKeysInState(ctx, private, inline); d.HasError() {
				t.Fatal(d)
			}
			if got := loadFingerprints(ctx, private); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("held %v, want %v", got, tt.want)
			}
		})
	}
}

// A write-only key not known until the apply may differ from the one the
// nodes were made with, so it plans a replacement, as `ssh_public_keys` does.
func TestAWriteOnlyKeyUnknownAtPlanReplaces(t *testing.T) {
	r := &clusterResource{}
	unknownKey := tftypes.NewValue(tftypes.Map{ElementType: tftypes.List{ElementType: tftypes.String}}, map[string]tftypes.Value{
		"ops": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}),
	})
	stateRaw, s := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": woUsers()}))
	configRaw, _ := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": woUsers(), "ssh_public_keys_wo": unknownKey}))
	plan := tfsdk.Plan{Raw: stateRaw, Schema: s.Schema}
	req := resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema}, Plan: plan,
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	freshPrivate(t, &req.Private, nil)
	if d := saveFingerprints(context.Background(), privateIn(t, req.Private), map[string][]string{"ops": {keyAFP}}); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.ModifyPlanResponse{Plan: plan}
	replaceOnChangedKeys(context.Background(), req, &resp)
	if len(resp.RequiresReplace) != 1 {
		t.Errorf("replaces %v, want ssh_access", resp.RequiresReplace)
	}
}

// Both resources plan references by username.
func TestBothResourcesPlanReferencesByUsername(t *testing.T) {
	for name, r := range map[string]resource.Resource{"cluster": &clusterResource{}, "instance group": &instanceGroupResource{}} {
		var s resource.SchemaResponse
		r.Schema(context.Background(), resource.SchemaRequest{}, &s)
		access, ok := s.Schema.Attributes["ssh_access"].(interface{ SetPlanModifiers() []planmodifier.Set })
		if !ok || !slices.ContainsFunc(access.SetPlanModifiers(), func(m planmodifier.Set) bool { _, is := m.(refsByUsername); return is }) {
			t.Errorf("%s: ssh_access doesn't plan references by username", name)
		}
	}
}

// A refresh holds the fingerprints of keys state holds as text, on both
// resources.
func TestARefreshHoldsTheKeysStateHolds(t *testing.T) {
	resp, _ := readCluster(t, sshAccessAPI(t, answerJSON(t, http.StatusOK,
		`[{"username":"ops","sshPublicKeyCredentials":["/v1/regions/test/projects/test/credentials/mcred-01jabcdefghjkmnpqrstvwxyz0"]}]`)), opsAccess())
	want := map[string][]string{"ops": {sshFingerprint("ssh-ed25519 AAAA ops")}}
	if got := loadFingerprints(context.Background(), privateIn(t, resp.Private)); !reflect.DeepEqual(got, want) {
		t.Errorf("cluster held %v, want %v", got, want)
	}
	body := deletingGroupBody("active")
	body["sshAccess"] = []any{map[string]any{"username": "ops",
		"sshPublicKeyCredentials": []any{"/v1/regions/test/projects/test/credentials/mcred-01jabcdefghjkmnpqrstvwxyz0"}}}
	read, _ := refreshThenPlan(t, body, with(opsGroup("active"), map[string]tftypes.Value{"ssh_access": opsAccess()}),
		with(opsGroup("active"), map[string]tftypes.Value{"ssh_access": opsAccess()}))
	if got := loadFingerprints(context.Background(), privateIn(t, read.Private)); !reflect.DeepEqual(got, want) {
		t.Errorf("group held %v, want %v", got, want)
	}
}

// A read right after a cluster create that answers no users, racing the
// platform, keeps the users the create sent.
func TestAClusterCreateKeepsItsUsersOverAnEmptyReadBack(t *testing.T) {
	var g groupsServer
	cluster, err := url.Parse(g.start(t).URL)
	if err != nil {
		t.Fatal(err)
	}
	rest := httputil.NewSingleHostReverseProxy(cluster)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ssh-access") {
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `[]`); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		rest.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	planRaw, s := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": opsAccess()}))
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
		Config: tfsdk.Config{Raw: planRaw, Schema: s.Schema}}, &resp)
	var stored types.Set
	resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("ssh_access"), &stored)...)
	if resp.Diagnostics.HasError() || stored.IsNull() || len(stored.Elements()) != 1 {
		t.Errorf("ssh_access = %v (%v), want the user the create sent", stored, resp.Diagnostics)
	}
}

// Keys the configuration leaves out keep what state holds for that user, so
// moving them to `ssh_public_keys_wo` plans no change. On a create there's
// nothing to keep, and they're planned null.
func TestKeysLeftOutKeepWhatStateHolds(t *testing.T) {
	ctx := context.Background()
	user := func(keys types.List) types.Set {
		v, d := types.SetValueFrom(ctx, sshUserObjectType(), []sshUserModel{{Username: types.StringValue("ops"),
			SSHPublicKeys: keys, Credentials: types.ListNull(types.StringType), Sudo: types.StringNull(), DisplayName: types.StringNull()}})
		if d.HasError() {
			t.Fatal(d)
		}
		return v
	}
	held := types.ListValueMust(types.StringType, []attr.Value{types.StringValue(keyA)})
	unknown := user(types.ListUnknown(types.StringType))
	leftOut := user(types.ListNull(types.StringType))
	for name, tt := range map[string]struct {
		state, config types.Set
		want          types.Set
	}{
		"kept from state": {user(held), leftOut, user(held)},
		"on a create":     {types.SetNull(sshUserObjectType()), leftOut, user(types.ListNull(types.StringType))},
		// Keys configured but not known until the apply stay unknown, on a
		// create and on a change alike.
		"configured, unknown, on a create": {types.SetNull(sshUserObjectType()), unknown, unknown},
		"configured, unknown, on a change": {user(held), unknown, unknown},
	} {
		t.Run(name, func(t *testing.T) {
			resp := planmodifier.SetResponse{PlanValue: unknown}
			refsByUsername{}.PlanModifySet(ctx, planmodifier.SetRequest{StateValue: tt.state, PlanValue: unknown, ConfigValue: tt.config}, &resp)
			if !resp.PlanValue.Equal(tt.want) {
				t.Errorf("planned %v, want %v", resp.PlanValue, tt.want)
			}
		})
	}
}

// Default tags not known until the apply still plan an update of `tags_all`
// when `ssh_access` is left out: only the unknowns the framework marks for
// that are reset.
func TestUnknownDefaultTagsStillPlanAnUpdate(t *testing.T) {
	r := &clusterResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}
	r.meta.DefaultTags = defaultTags{unknown: true}
	base := with(existingCluster(nil), map[string]tftypes.Value{"state": str("active"), "on_failed": str("keep"),
		"tags_all": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{})})
	stateRaw, s := testValue(t, r, with(base, map[string]tftypes.Value{"ssh_access": opsAccess()}))
	planRaw, _ := testValue(t, r, with(base, map[string]tftypes.Value{"ssh_access": opsAccess(),
		"state": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}))
	configRaw, _ := testValue(t, r, with(base, map[string]tftypes.Value{"state": tftypes.NewValue(tftypes.String, nil),
		"tags_all": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)}))
	plan := tfsdk.Plan{Raw: planRaw, Schema: s.Schema}
	resp := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
		Plan: plan, State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}, &resp)
	var all types.Map
	resp.Diagnostics.Append(resp.Plan.GetAttribute(context.Background(), path.Root("tags_all"), &all)...)
	if resp.Diagnostics.HasError() || !all.IsUnknown() {
		t.Errorf("tags_all planned %v (%v), want unknown: the defaults aren't known yet", all, resp.Diagnostics)
	}
}

// References configured but not known until the apply, such as a credential
// created in the same apply, stay unknown rather than taking state's.
func TestConfiguredUnknownReferencesStayUnknown(t *testing.T) {
	ctx := context.Background()
	user := func(refs types.List) types.Set {
		v, d := types.SetValueFrom(ctx, sshUserObjectType(), []sshUserModel{{Username: types.StringValue("ops"),
			SSHPublicKeys: types.ListNull(types.StringType), Credentials: refs, Sudo: types.StringNull(), DisplayName: types.StringNull()}})
		if d.HasError() {
			t.Fatal(d)
		}
		return v
	}
	unknown := user(types.ListUnknown(types.StringType))
	state := user(types.ListValueMust(types.StringType, []attr.Value{types.StringValue("/v1/regions/r/projects/p/credentials/mcred-0123456789abcdefghjkmnpqrs")}))
	resp := planmodifier.SetResponse{PlanValue: unknown}
	refsByUsername{}.PlanModifySet(ctx, planmodifier.SetRequest{StateValue: state, PlanValue: unknown, ConfigValue: unknown}, &resp)
	if !resp.PlanValue.Equal(unknown) {
		t.Errorf("planned %v, want the references unknown", resp.PlanValue)
	}
}

// privateIn is a response's or request's private state, which these tests
// both read and write.
func privateIn(t *testing.T, p any) interface {
	privateState
	privateWriter
} {
	t.Helper()
	rw, ok := p.(interface {
		privateState
		privateWriter
	})
	if !ok {
		t.Fatal("private state can't be read and written")
	}
	return rw
}
