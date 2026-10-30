package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// How a resource's stored state survives a change to its shape.
//
// Terraform stores each resource instance with the schema version the provider
// declared when it wrote it. On the first plan after a provider upgrade it
// hands that state back with its version, and the provider has to return it in
// the current shape. The framework does that only between declared versions,
// so a removal, a rename or a type change with no version to migrate from is a
// state nobody can read.
//
// An added attribute is the exception. At the same version the framework
// decodes a stored state with any attribute it doesn't know skipped and any it
// misses read as null (terraform-plugin-framework's
// internal/fwserver/server_upgraderesourcestate.go, and tftypes'
// value_json.go). So this build reads a state the last release wrote, and the
// last release reads one this build wrote, as long as the version stays. A
// version raised for an addition alone would lose the second: Terraform
// refuses a state from a newer schema version, so nobody could go back to the
// last release.
//
// Each resource's history is the list of steps its state has taken, and its
// schema version is the length of that list. So the version can't be raised
// without writing the step that migrates to it, and the snapshot test in
// state_history_internal_test.go fails on any shape change that doesn't raise
// it, other than the additions a step lists in addedLater. Together they mean
// a shape change can't ship without a migration.
//
// A step rewrites the state as Terraform stores it, a JSON object, rather than
// decoding it through a frozen copy of the old schema. Upgrading from version N
// runs every step from N onwards, so each step only ever has to know the one
// version before it, and an old state reaches the current shape however many
// releases it skipped.

// stateStep migrates a resource's stored state from one schema version to the
// next.
type stateStep struct {
	// why says what changed, in a sentence. It's required, because a reader
	// of the history needs to know what each version means.
	why string

	// apply rewrites the state in place. Numbers arrive as json.Number so
	// that none loses precision on the way through. Nil means the step
	// changed nothing a stored value has to be rewritten for: the version
	// moved, or an attribute was added (it reads as null) or removed (it's
	// dropped, and has to be named in dropped).
	apply func(state map[string]any) error

	// dropped and moved account for every attribute that leaves the stored
	// shape at this step, by its path in the state ("timeouts.delete" for one
	// inside a block). An attribute in dropped is discarded on purpose. One
	// in moved is carried by apply to the path it maps to. A rename written
	// as a remove and an add, with nothing to carry the value, would
	// otherwise pass every test and quietly empty the attribute in every
	// upgraded state. So the snapshot test fails on an attribute that goes
	// without being named in one of them, and checks that apply really puts
	// each moved value where moved says.
	dropped []string
	moved   map[string]string

	// addedLater is the attributes added to the shape this step produces
	// after a release stored it, with no step of their own: each one's path,
	// as dropped names it, and its type in Terraform's type notation, as the
	// snapshots write it ("string", ["list","string"]). New ones go on the
	// newest step only, since an older version's shape is what releases
	// stored. The snapshot test checks the schema against that version's
	// snapshot plus these, so an added attribute that changes type fails it,
	// and one that leaves at a later step has to be accounted for there like
	// any other.
	addedLater map[string]string
}

// formerName is a type a resource was registered under before it was renamed.
// A `moved` block from that type name lands on the resource, through MoveState.
type formerName struct {
	// providerType is the last segment of the address of the provider that
	// registered the old name. Only that segment is compared, so the move
	// works from whichever local mirror or dev_overrides namespace the old
	// build was installed under. A state already handed to this provider
	// with `terraform state replace-provider` is accepted too: see
	// formerlyNamed.
	providerType string
	typeName     string
}

// preReleaseProviderTypeName is the provider type the cluster and the instance
// group were registered under before the name settled. It was only ever
// installed through dev_overrides, under a developer's own namespace.
const preReleaseProviderTypeName = "k0rdentai"

func preReleaseName(suffix string) formerName {
	return formerName{
		providerType: preReleaseProviderTypeName,
		typeName:     preReleaseProviderTypeName + resourceNameSeparator + suffix,
	}
}

type stateHistory struct {
	steps       []stateStep
	formerNames []formerName

	// firstVersion is the schema version the resource's first release
	// declared. It's 0 for the resources that predate declared versions. A
	// resource added since starts with versionDeclared as its only step and
	// firstVersion 1: no build ever stored it at 0, so there's no v0.json to
	// keep and no upgrader from 0. It's set once, when the resource is added,
	// and never raised, because a state at a version it skipped would have
	// nothing to upgrade it.
	firstVersion int64
}

// versionDeclared is every resource's first step. Before it, no resource
// declared a schema version, so every state a pre-release build wrote is at
// version 0. Declaring 1 changes no stored value. It exists so that the first
// real shape change has a declared version to migrate from, and so that the
// upgrade path runs, and is tested, from the first release on rather than
// first running on somebody's state the day it matters.
var versionDeclared = stateStep{
	why: "The provider declares schema versions from here on. The shape is the one version 0 already had.",
}

// tagsAllAdded is the step each tagged resource took when the provider gained
// default_tags (tags.go). Until then there were no defaults to merge, so the
// platform held exactly the configured tags, and that's what tags_all starts
// as. Without it, an upgraded state would hold a null tags_all, and a plan
// run with -refresh=false would propose an update to fill it.
var tagsAllAdded = stateStep{
	why: "Adds the computed `tags_all`, the tags the platform holds with the provider's `default_tags` " +
		"merged in. A state from before it had no defaults to merge, so `tags_all` starts as its `tags`, " +
		"or empty, and the next refresh reads it from the platform.",
	apply: func(state map[string]any) error {
		// A null or absent `tags` is no tags.
		tags, ok := state["tags"].(map[string]any)
		if !ok {
			tags = nil
		}
		all := make(map[string]any, len(tags))
		for k, v := range tags {
			all[k] = v
		}
		state[tagsAllAttribute] = all
		return nil
	},
}

// stateHistories is every resource's history, by suffix. A registered resource
// with no entry here fails TestEveryResourceDeclaresItsStateHistory.
//
// To change a resource's stored shape, append a step to its entry, naming any
// attribute that leaves the shape in the step's dropped or moved. The
// cluster and the instance group were registered under the pre-release
// provider type until the name settled, so a `moved` block from those names
// works too.
var stateHistories = map[string]stateHistory{
	clusterResourceSuffix: {
		steps: []stateStep{versionDeclared, {
			why: "Adds the computed `kubernetes_version`, the Kubernetes version the bound ClusterType " +
				"version carries. A state from before it reads null there until the next refresh fills it.",
		}, {
			why: "Adds `storage_ceiling_gib`, the storage ceiling the API lets a cluster " +
				"declare. A state from before it reads null there until the next refresh fills it, and a " +
				"cluster that declares none keeps the null.",
		}, {
			why: "Adds `on_destroy_busy`, what a destroy does with a cluster mid-operation. A state from " +
				"before it reads null there, which means `cancel`, what every destroy did before.",
		}, tagsAllAdded, {
			why: "Adds `ssh_access`, the users written onto every node at create. A state from before it " +
				"reads null there. Nothing records what such a cluster was created with, so a value added " +
				"later is recorded in place, without a replacement, and the plan warns that the nodes keep " +
				"their users.",
			// What a plan does with a cluster the platform reports `failed`.
			// A state the last release wrote reads null there, which means
			// `wait`.
			addedLater: map[string]string{"on_failed": `"string"`},
		}},
		formerNames: []formerName{preReleaseName(clusterResourceSuffix)},
	},
	instanceGroupResourceSuffix: {
		steps:       []stateStep{versionDeclared},
		formerNames: []formerName{preReleaseName(instanceGroupResourceSuffix)},
	},
	networkResourceSuffix:           {steps: []stateStep{versionDeclared, tagsAllAdded}},
	subnetResourceSuffix:            {steps: []stateStep{versionDeclared}},
	securityGroupResourceSuffix:     {steps: []stateStep{versionDeclared, tagsAllAdded}},
	vpcPeeringResourceSuffix:        {steps: []stateStep{versionDeclared, tagsAllAdded}},
	vpcSecurityGroupsResourceSuffix: {steps: []stateStep{versionDeclared}},
	fileStoreResourceSuffix:         {steps: []stateStep{versionDeclared}},
	projectResourceSuffix: {steps: []stateStep{versionDeclared, {
		why: "Adds `region`, the region the API locks a project to. A state " +
			"from before it reads null there, and the refresh that follows the upgrade fills it from the " +
			"project. A project the platform reports with no region keeps the null.",
	}}},
	webhookSubscriptionResourceSuffix: {
		steps:        []stateStep{versionDeclared},
		firstVersion: 1,
	},
}

// version is the schema version a resource declares: one per step taken.
func (h stateHistory) version() int64 { return int64(len(h.steps)) }

// addedAt is what was added to version v's shape after a release stored it.
func (h stateHistory) addedAt(v int64) map[string]string {
	if v < 1 || v > h.version() {
		return nil
	}
	return h.steps[v-1].addedLater
}

// FirstStateVersion is the earliest schema version any build stored the
// resource typeName at, so the earliest a state Terraform hands back can be.
// The conformance run upgrades a state from every version from there on.
func FirstStateVersion(typeName string) int64 {
	return stateHistories[strings.TrimPrefix(typeName, ResourcePrefix())].firstVersion
}

// shapeUnchangedSince reports whether no step from version v onwards rewrites
// a stored value, so that a state at v reads unchanged at the current version.
func (h stateHistory) shapeUnchangedSince(v int64) bool {
	for _, step := range h.steps[v:] {
		if step.apply != nil {
			return false
		}
	}
	return true
}

// upgraders is a resource's UpgradeState: one entry per prior version a
// release could have stored it at, each running every step from there to now.
func (h stateHistory) upgraders() map[int64]resource.StateUpgrader {
	out := make(map[int64]resource.StateUpgrader, len(h.steps))
	for v := h.firstVersion; v < h.version(); v++ {
		out[v] = resource.StateUpgrader{
			StateUpgrader: func(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
				resp.Diagnostics.Append(h.migrate(ctx, req.RawState, v, &resp.State)...)
			},
		}
	}
	return out
}

// movers is a resource's MoveState. A request from anything but one of its
// former names is left alone, which the framework turns into an error naming
// the source, so a moved block can't turn one kind of resource into another.
func (h stateHistory) movers() []resource.StateMover {
	return []resource.StateMover{{
		StateMover: func(ctx context.Context, req resource.MoveStateRequest, resp *resource.MoveStateResponse) {
			if !h.formerlyNamed(req.SourceProviderAddress, req.SourceTypeName) {
				return
			}
			resp.Diagnostics.Append(h.migrate(ctx, req.SourceRawState, req.SourceSchemaVersion, &resp.TargetState)...)
			// The old build was this provider, so whatever it kept in
			// private state means the same thing here.
			resp.TargetPrivate = req.SourcePrivate
		},
	}}
}

// formerlyNamed reports whether a moved block's source is one of the
// resource's former names. The source can be held by the old provider or by
// this one. Terraform won't plan while the state names a provider it can't
// resolve, even one it only moves out of, so the simplest route off a
// pre-release state is to hand it to this provider with `terraform state
// replace-provider` first, and that leaves the old type name under this
// provider's address.
func (h stateHistory) formerlyNamed(providerAddress, typeName string) bool {
	providerType := providerAddress[strings.LastIndex(providerAddress, "/")+1:]
	for _, f := range h.formerNames {
		if f.typeName == typeName && (providerType == f.providerType || providerType == ProviderTypeName) {
			return true
		}
	}
	return false
}

// migrate brings a stored state from version from to the current shape and
// writes it to out, whose schema the framework has already set to the current
// one.
func (h stateHistory) migrate(ctx context.Context, raw *tfprotov6.RawState, from int64, out *tfsdk.State) diag.Diagnostics {
	var diags diag.Diagnostics
	if from > h.version() {
		diags.AddError("Unable to upgrade resource state",
			fmt.Sprintf("The state is at schema version %d and this build knows versions %d to %d. "+
				"A newer build of the provider wrote it, and this one can't read it.", from, h.firstVersion, h.version()))
		return diags
	}
	if from < h.firstVersion {
		diags.AddError("Unable to upgrade resource state",
			fmt.Sprintf("The state is at schema version %d, and no build of the provider ever stored this resource "+
				"below version %d, so it wasn't written by this provider.", from, h.firstVersion))
		return diags
	}
	if raw == nil || raw.JSON == nil {
		// Flatmap state is what Terraform 0.11 and earlier stored, and no
		// build of this provider has ever been run by one.
		diags.AddError("Unable to upgrade resource state",
			"The state isn't stored as JSON, so it predates Terraform 0.12 and no build of this provider wrote it.")
		return diags
	}

	dec := json.NewDecoder(bytes.NewReader(raw.JSON))
	dec.UseNumber()
	var state map[string]any
	if err := dec.Decode(&state); err != nil {
		diags.AddError("Unable to upgrade resource state",
			fmt.Sprintf("The version %d state isn't a JSON object: %v", from, err))
		return diags
	}
	for v := from; v < h.version(); v++ {
		step := h.steps[v]
		if step.apply == nil {
			continue
		}
		if err := step.apply(state); err != nil {
			diags.AddError("Unable to upgrade resource state",
				fmt.Sprintf("Migrating the state from schema version %d to %d (%s) failed: %v. "+
					"This is a bug in the provider. The state file hasn't been changed.", v, v+1, step.why, err))
			return diags
		}
	}

	upgraded, err := json.Marshal(state)
	if err != nil {
		diags.AddError("Unable to upgrade resource state", err.Error())
		return diags
	}
	// Attributes the current schema doesn't have are dropped, which is what
	// the framework does to a same-version state too. Pre-release builds
	// wrote attributes that have since gone, and a state carrying one has to
	// stay readable.
	value, err := tftypes.ValueFromJSONWithOpts(upgraded, out.Schema.Type().TerraformType(ctx),
		tftypes.ValueFromJSONOpts{IgnoreUndefinedAttributes: true})
	if err != nil {
		diags.AddError("Unable to upgrade resource state",
			fmt.Sprintf("After migrating from schema version %d the state doesn't fit the current schema: %v. "+
				"This is a bug in the provider: the shape changed without a step that migrates it.", from, err))
		return diags
	}
	out.Raw = value
	return diags
}
