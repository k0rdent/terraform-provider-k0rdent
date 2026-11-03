package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// credentialsServer answers SSH key credential reads as the k0rdent API
// v1.3.0-rc.5 did on kind: each key's fingerprint to an account that may read
// them, the gateway's bare 403 to one that may not, and `REVOKED` with the
// revocation as its operation for a revoked key.
type credentialsServer struct {
	fingerprints map[string]string // by credential id
	revoked      map[string]bool
	refused      bool
	reads        int
}

func (c *credentialsServer) start(t *testing.T) string {
	t.Helper()
	const prefix = "/v1/regions/test/projects/test/credentials/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutPrefix(r.URL.Path, prefix)
		if r.Method != http.MethodGet || !ok {
			writeTestError(t, w, http.StatusNotFound, "not found")
			return
		}
		c.reads++
		if c.refused {
			writeTestError(t, w, http.StatusForbidden, "forbidden")
			return
		}
		fp, ok := c.fingerprints[id]
		if !ok {
			writeTestError(t, w, http.StatusNotFound, "not found: credential")
			return
		}
		body := map[string]any{"id": id, "type": "ssh_public_key", "scope": "project", "state": "ACTIVE",
			"currentVersion": 1, "materialAttributes": map[string]string{"fingerprint": fp}}
		if c.revoked[id] {
			body["state"] = "REVOKED"
			body["operation"] = map[string]any{"type": "revocation", "status": "ok"}
		}
		writeTestJSON(t, w, http.StatusOK, body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

const (
	credA = "mcred-0123456789abcdefghjkmnpqra"
	credB = "mcred-0123456789abcdefghjkmnpqrb"
)

func credRef(id string) string { return "/v1/regions/test/projects/test/credentials/" + id }

// opsWithRefs is `ssh_access` with ops and the given references, as a read
// after an import holds it.
func opsWithRefs(t *testing.T, ids ...string) types.Set {
	t.Helper()
	refs := make([]attr.Value, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, types.StringValue(credRef(id)))
	}
	u := sshUserModel{Username: types.StringValue("ops"), SSHPublicKeys: types.ListNull(types.StringType),
		Credentials: types.ListValueMust(types.StringType, refs), Sudo: types.StringNull(), DisplayName: types.StringNull()}
	set, d := types.SetValueFrom(context.Background(), sshUserObjectType(), []sshUserModel{u})
	if d.HasError() {
		t.Fatal(d)
	}
	return set
}

// A refresh reads each user's fingerprints from the platform and holds them,
// over any held, in the references' order. One the account can't read keeps
// what's held; with nothing held it's recorded with the reason, and on an
// import it's a warning too. A revoked key is recorded, not held.
func TestARefreshHoldsTheFingerprintsThePlatformHolds(t *testing.T) {
	for name, tt := range map[string]struct {
		api           credentialsServer
		held          map[string][]string
		refs          []string
		imported      bool
		wantHeld      map[string][]string
		wantUnchecked string
		wantRevoked   []string
		wantWarnings  int
	}{
		"read after an import": {
			api:  credentialsServer{fingerprints: map[string]string{credA: keyAFP}},
			refs: []string{credA}, imported: true,
			wantHeld: map[string][]string{"ops": {keyAFP}},
		},
		"read in the references' order": {
			api:      credentialsServer{fingerprints: map[string]string{credA: keyAFP, credB: keyBFP}},
			refs:     []string{credB, credA},
			wantHeld: map[string][]string{"ops": {keyBFP, keyAFP}},
		},
		"the platform's over what the create held": {
			api:  credentialsServer{fingerprints: map[string]string{credA: keyBFP}},
			held: map[string][]string{"ops": {keyAFP}}, refs: []string{credA},
			wantHeld: map[string][]string{"ops": {keyBFP}},
		},
		"refused after an import": {
			api:  credentialsServer{refused: true},
			refs: []string{credA}, imported: true,
			wantHeld: map[string][]string{}, wantUnchecked: "HTTP 403", wantWarnings: 1,
		},
		"refused on a later refresh": {
			api:      credentialsServer{refused: true},
			refs:     []string{credA},
			wantHeld: map[string][]string{}, wantUnchecked: "HTTP 403",
		},
		"refused with fingerprints held from the create": {
			api:  credentialsServer{refused: true},
			held: map[string][]string{"ops": {keyAFP}}, refs: []string{credA},
			wantHeld: map[string][]string{"ops": {keyAFP}},
		},
		"a credential gone": {
			api:  credentialsServer{fingerprints: map[string]string{}},
			refs: []string{credA}, imported: true,
			wantHeld: map[string][]string{}, wantUnchecked: "HTTP 404", wantWarnings: 1,
		},
		"revoked after an import": {
			api:  credentialsServer{fingerprints: map[string]string{credA: keyAFP}, revoked: map[string]bool{credA: true}},
			refs: []string{credA}, imported: true,
			wantHeld: map[string][]string{"ops": {keyAFP}}, wantRevoked: []string{"ops"},
		},
		"revoked": {
			api:  credentialsServer{fingerprints: map[string]string{credA: keyAFP}, revoked: map[string]bool{credA: true}},
			held: map[string][]string{"ops": {keyAFP}}, refs: []string{credA},
			wantHeld: map[string][]string{"ops": {keyAFP}}, wantRevoked: []string{"ops"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			meta := testClusterMeta(t, tt.api.start(t), time.Minute)
			var resp resource.ReadResponse
			freshPrivate(t, &resp.Private, nil)
			private := privateIn(t, resp.Private)
			if tt.held != nil {
				if d := saveFingerprints(ctx, private, tt.held); d.HasError() {
					t.Fatal(d)
				}
			}
			diags := holdPlatformFingerprints(ctx, meta, private, opsWithRefs(t, tt.refs...), tt.imported)
			if diags.HasError() || diags.WarningsCount() != tt.wantWarnings {
				t.Errorf("diagnostics %v, want %d warnings", diags, tt.wantWarnings)
			}
			if got := loadFingerprints(ctx, private); !reflect.DeepEqual(got, tt.wantHeld) {
				t.Errorf("held %v, want %v", got, tt.wantHeld)
			}
			why := loadUnchecked(ctx, private)["ops"]
			if (tt.wantUnchecked == "") != (why == "") || !strings.Contains(why, tt.wantUnchecked) {
				t.Errorf("unchecked %q, want %q", why, tt.wantUnchecked)
			}
			if got := loadRevoked(ctx, private); len(got) != len(tt.wantRevoked) || (len(got) > 0 && !reflect.DeepEqual(got, tt.wantRevoked)) {
				t.Errorf("revoked %v, want %v", got, tt.wantRevoked)
			}
		})
	}
}

// After an import, a plan checks the configured keys, in either layout,
// against the fingerprints a refresh read from the platform: the same keys in
// any order plan nothing, and another key replaces. Keys with none read plan
// an update with a warning naming the reason and the role, and the apply
// stops on it, so the plan never shows no change over them and a destroy's
// plan still runs. Nothing is ever recorded unchecked.
func TestAnImportedResourcesKeysAreCheckedAgainstThePlatform(t *testing.T) {
	ctx := context.Background()
	both := map[string][]string{"ops": {keyAFP, keyBFP}}
	for name, tt := range map[string]struct {
		writeOnly, text  []string
		held             map[string][]string
		why              map[string]string
		replace, blocked bool
	}{
		"write-only, the platform's key":        {writeOnly: []string{keyA}, held: map[string][]string{"ops": {keyAFP}}},
		"write-only, another key":               {writeOnly: []string{keyB}, held: map[string][]string{"ops": {keyAFP}}, replace: true},
		"write-only, the keys in another order": {writeOnly: []string{keyB, keyA}, held: both},
		"write-only, one key of two":            {writeOnly: []string{keyA}, held: both, replace: true},
		"write-only, none read":                 {writeOnly: []string{keyA}, why: map[string]string{"ops": "HTTP 403 reading x"}, blocked: true},
		"text, none read":                       {text: []string{keyA}, why: map[string]string{"ops": "HTTP 403 reading x"}, blocked: true},
		"text, the platform's key":              {text: []string{keyA}, held: map[string][]string{"ops": {keyAFP}}},
		"text, another key":                     {text: []string{keyB}, held: map[string][]string{"ops": {keyAFP}}, replace: true},
		"no keys configured, none read":         {why: map[string]string{"ops": "HTTP 403 reading x"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := &clusterResource{}
			stateRaw, s := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": refsValue(credA)}))
			cfg := map[string]tftypes.Value{"ssh_access": refsValue(credA)}
			if tt.text != nil {
				cfg["ssh_access"] = opsWith(tftypes.NewValue(tftypes.String, nil), tt.text...)
			}
			if tt.writeOnly != nil {
				cfg["ssh_access"] = woUsers()
				cfg["ssh_public_keys_wo"] = woKeys(tt.writeOnly...)
			}
			configRaw, _ := testValue(t, r, existingCluster(cfg))
			plan := tfsdk.Plan{Raw: stateRaw, Schema: s.Schema}
			req := resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema}, Plan: plan,
				State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
			freshPrivate(t, &req.Private, nil)
			if tt.held != nil {
				if d := saveFingerprints(ctx, privateIn(t, req.Private), tt.held); d.HasError() {
					t.Fatal(d)
				}
			}
			if tt.why != nil {
				if d := setUnchecked(ctx, privateIn(t, req.Private), tt.why); d.HasError() {
					t.Fatal(d)
				}
			}
			resp := resource.ModifyPlanResponse{Plan: plan, Private: req.Private}
			replaceOnChangedKeys(ctx, req, &resp)
			if got := len(resp.RequiresReplace) == 1; got != tt.replace {
				t.Errorf("replaces = %v, want %v: %v", resp.RequiresReplace, tt.replace, resp.Diagnostics)
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("the plan failed: %v", resp.Diagnostics)
			}
			warns := resp.Diagnostics.Warnings()
			if got := len(warns) == 1; got != tt.blocked {
				t.Fatalf("warned = %v, want %v: %v", got, tt.blocked, resp.Diagnostics)
			}
			if got := !resp.Plan.Raw.Equal(plan.Raw); got != (tt.blocked || tt.replace) {
				t.Errorf("planned a change = %v, want %v", got, tt.blocked || tt.replace)
			}
			var applied diag.Diagnostics
			if got := refuseUncheckedKeys(ctx, privateIn(t, resp.Private), &applied); got != tt.blocked {
				t.Errorf("the apply stops = %v, want %v", got, tt.blocked)
			}
			if tt.blocked {
				for _, want := range []string{"HTTP 403", "project", "credentials-admin", "organization-scoped"} {
					if !strings.Contains(applied[0].Detail(), want) {
						t.Errorf("the apply's error doesn't say %q:\n%s", want, applied[0].Detail())
					}
				}
				if !strings.Contains(warns[0].Detail(), "A destroy isn't affected") {
					t.Errorf("the warning doesn't say a destroy runs:\n%s", warns[0].Detail())
				}
			}
		})
	}
}

// A key whose credential the platform revoked is a warning on every plan,
// never a replacement: the nodes still take it, and replacing would put the
// same key back.
func TestARevokedKeyWarnsAndPlansNothing(t *testing.T) {
	ctx := context.Background()
	r := &clusterResource{}
	stateRaw, s := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": refsValue(credA)}))
	configRaw, _ := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": woUsers(), "ssh_public_keys_wo": woKeys(keyA)}))
	plan := tfsdk.Plan{Raw: stateRaw, Schema: s.Schema}
	req := resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema}, Plan: plan,
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	freshPrivate(t, &req.Private, nil)
	private := privateIn(t, req.Private)
	if d := saveFingerprints(ctx, private, map[string][]string{"ops": {keyAFP}}); d.HasError() {
		t.Fatal(d)
	}
	if d := private.SetKey(ctx, sshKeysRevokedKey, []byte(`["ops"]`)); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.ModifyPlanResponse{Plan: plan, Private: req.Private}
	replaceOnChangedKeys(ctx, req, &resp)
	if len(resp.RequiresReplace) != 0 || resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 ||
		!strings.Contains(resp.Diagnostics.Warnings()[0].Detail(), "still accept it") {
		t.Errorf("replaces %v, diagnostics %v, want one warning and no replacement", resp.RequiresReplace, resp.Diagnostics)
	}
	if !resp.Plan.Raw.Equal(plan.Raw) {
		t.Errorf("the plan changed: %v", resp.Plan.Raw)
	}
}

// refsValue is `ssh_access` with ops, no keys and the given references.
func refsValue(ids ...string) tftypes.Value {
	refs := make([]tftypes.Value, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, str(credRef(id)))
	}
	return tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{tftypes.NewValue(sshUserTF, map[string]tftypes.Value{
		"username":            str("ops"),
		"ssh_public_keys":     tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, nil),
		"ssh_public_key_refs": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, refs),
		"sudo":                tftypes.NewValue(tftypes.String, nil),
		"display_name":        tftypes.NewValue(tftypes.String, nil),
	})})
}

func setUnchecked(ctx context.Context, private privateWriter, why map[string]string) diag.Diagnostics {
	b, err := json.Marshal(why)
	if err != nil {
		panic(err)
	}
	return private.SetKey(ctx, sshKeysUncheckedKey, b)
}

// The error names the role only where a read was refused, or nothing says why:
// a credential that's gone needs no role.
func TestTheUncheckedKeysTextNamesTheRoleOnlyWhenARoleWouldHelp(t *testing.T) {
	for name, tt := range map[string]struct {
		why  map[string]string
		role bool
	}{
		"refused":       {map[string]string{"ops": "HTTP 403 reading x"}, true},
		"none recorded": {nil, true},
		"gone":          {map[string]string{"ops": "HTTP 404 reading x"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			text := uncheckedKeysText([]string{"ops"}, tt.why)
			if got := strings.Contains(text, "credentials-admin"); got != tt.role {
				t.Errorf("names the role = %v, want %v:\n%s", got, tt.role, text)
			}
		})
	}
}
