package provider

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The replacement guard.
//
// A change that cannot be made in place is planned by Terraform as a destroy
// and a create, and the only sign of it is "must be replaced" in a plan that an
// automated pipeline may never show a person. On a resource that holds tenant
// data or runs tenant workloads that is the failure to prevent: a silent
// replacement. So on those resources the plan fails instead, names the
// attribute that forces the replacement, and says how to permit it.
//
// # The opt-in is an attribute, read from configuration
//
// `allow_replace = true` on the resource permits the replacement. It is read
// from the configuration being planned, not from state, so the same plan that
// makes the change can carry the permission, and removing the line puts the
// guard back on the next plan. It is an ordinary stored attribute rather than a
// write-only one, because write-only attributes need Terraform 1.11 and this
// provider supports 1.10. The cost is that setting or removing it on its own is
// an in-place update. It changes nothing on the platform, and when the plan
// leaves nothing else to fill in, the guard answers it without calling the
// resource at all.
//
// Terraform's own `lifecycle { prevent_destroy = true }` is the other way round:
// protection the configuration has to ask for, and invisible to a provider.
// This guard is on by default. `terraform apply -replace=ADDR` and
// `terraform taint` are not refused, because each is an explicit request for
// exactly this replacement and neither changes an attribute.
//
// # Why the guard runs in ModifyPlan, and asks the modifiers again
//
// The framework collects RequiresReplace from attribute plan modifiers before a
// resource's ModifyPlan runs, and does not show ModifyPlan which paths it
// collected. It also runs an object's own modifiers before the modifiers of the
// attributes inside it, and a modifier such as UseStateForUnknown can put a
// prior value back after a RequiresReplace has already said yes to an unknown.
// Terraform replaces only when the final planned value differs, so a guard that
// trusted the first yes would refuse changes Terraform makes in place.
//
// So the guard runs from ModifyPlan, when every attribute has its final planned
// value, and asks each attribute's replacement modifiers again against that
// value. That is the answer Terraform reaches, so an in-place change is never
// refused. Region and project are decided in ModifyPlan itself (scope.go), and
// arrive in the response's RequiresReplace.
//
// # One seam
//
// A resource opts in by being registered through guardReplacements, which owns
// the attribute, its documentation, the refusal and the value in state. See
// replace_guard_resource.go.

// allowReplaceAttribute is the opt-in's name on every guarded resource.
const allowReplaceAttribute = "allow_replace"

// tenantState is what a guarded resource holds and what a replacement loses.
type tenantState struct {
	// noun is the resource as a reader names it: "cluster", "file store".
	noun string
	// loses says what a replacement destroys, as a sentence.
	loses string
}

var (
	clusterTenantState = tenantState{noun: "cluster",
		loses: "A cluster runs tenant workloads, and a replacement deletes it, its nodes and " +
			"everything scheduled on them before a new, empty cluster is created."}
	instanceGroupTenantState = tenantState{noun: "instance group",
		loses: "An instance group's hosts run tenant workloads, and a replacement deprovisions " +
			"every host in it before new ones are provisioned."}
	fileStoreTenantState = tenantState{noun: "file store",
		loses: "A file store holds tenant data, and a replacement deletes the store and the data " +
			"on it before an empty one is created."}
	subnetTenantState = tenantState{noun: "subnet",
		loses: "A subnet holds the address allocation the workloads on it use. The API has no " +
			"update for a subnet, so any change deletes it and allocates a new one from the " +
			"pool, which can get a different CIDR and gateway."}
	networkTenantState = tenantState{noun: "network",
		loses: "A network carries the subnets and interfaces tenant workloads attach to, and a " +
			"replacement deletes it and allocates a new fabric segment."}
	projectTenantState = tenantState{noun: "project",
		loses: "A project contains the tenant's clusters, networks, subnets and API keys, and a " +
			"replacement deletes it and creates an empty one."}
)

// allowReplaceSchemaAttribute is the opt-in. Optional and not Computed: left
// out, it stays null in state, so no existing configuration plans a change
// when it appears in the schema.
func allowReplaceSchemaAttribute(what tenantState) schema.BoolAttribute {
	return schema.BoolAttribute{
		Optional: true,
		MarkdownDescription: fmt.Sprintf("Permit a plan that replaces this %[1]s. Left out or `false`, "+
			"a change that cannot be made in place fails the plan, naming the attribute that forces "+
			"the replacement, instead of destroying the %[1]s and creating another. Set it to `true` "+
			"in the same configuration as the change to let the replacement go ahead, and remove it "+
			"afterwards to put the guard back. It is read from the configuration being planned and "+
			"must be known at plan time. It is never sent to the platform: setting or removing it on "+
			"its own is an update that changes nothing there.", what.noun),
	}
}

// declareReplacementGuard adds the opt-in to a tenant-state resource's schema
// and says in its description which attributes replace it. The guarded
// resource's Schema calls it after the resource's own. It also writes each
// attribute's schema facts into its description (schema_docs.go), so the
// attribute reference marks what replaces it too.
func declareReplacementGuard(s *schema.Schema, what tenantState) {
	s.MarkdownDescription += replacementParagraph(*s, what)
	s.Attributes[allowReplaceAttribute] = allowReplaceSchemaAttribute(what)
	documentResourceSchema(s)
}

// describeReplacement is the documentation half alone, for a resource that
// holds no tenant state and so is replaced without asking.
func describeReplacement(s *schema.Schema, noun string) {
	s.MarkdownDescription += replacementParagraph(*s, tenantState{noun: noun})
	documentResourceSchema(s)
}

// replacementParagraph lists what replaces the resource, region and project
// included where the resource has them.
func replacementParagraph(s schema.Schema, what tenantState) string {
	replacing := ReplacingAttributes(s)
	if len(replacing) == 0 {
		return ""
	}
	quoted := make([]string, len(replacing))
	for i, name := range replacing {
		quoted[i] = "`" + name + "`"
	}
	list := quoted[0]
	if n := len(quoted); n > 1 {
		list = strings.Join(quoted[:n-1], ", ") + " or " + quoted[n-1]
	}
	out := fmt.Sprintf("\n\n**What replaces it.** Changing %s cannot be done in place and replaces "+
		"the %s.", list, what.noun)
	if what.loses == "" {
		return out + " Everything else changes in place."
	}
	return out + fmt.Sprintf(" %s So a plan that would replace it fails and names the attribute, "+
		"unless `%s = true` is set on the resource. Everything else changes in place.",
		what.loses, allowReplaceAttribute)
}

// ReplacingAttributes lists the attributes whose change can replace a
// resource: every attribute carrying a RequiresReplace modifier, at the top
// level or inside a single nested object, and region and project where the
// resource has them. Sorted.
func ReplacingAttributes(s schema.Schema) []string {
	var out []string
	for _, scope := range []string{"region", "project"} {
		if _, ok := s.Attributes[scope]; ok {
			out = append(out, scope)
		}
	}
	var walk func(attrs map[string]schema.Attribute, parent string)
	walk = func(attrs map[string]schema.Attribute, parent string) {
		for name, a := range attrs {
			if len(replacementModifiers(a)) > 0 {
				out = append(out, parent+name)
			}
			if nested, ok := a.(schema.SingleNestedAttribute); ok {
				walk(nested.Attributes, parent+name+".")
			}
		}
	}
	walk(s.Attributes, "")
	slices.Sort(out)
	return out
}

// UnguardableReplacements lists replacement modifiers the guard cannot reach:
// ones inside a list, set or map of nested objects, whose paths depend on the
// elements. None exists today, and TestNoReplacementTheGuardCannotReach keeps
// it that way.
func UnguardableReplacements(s schema.Schema) []string {
	var out []string
	var walk func(attrs map[string]schema.Attribute, parent string, inCollection bool)
	walk = func(attrs map[string]schema.Attribute, parent string, inCollection bool) {
		for name, a := range attrs {
			if inCollection && len(replacementModifiers(a)) > 0 {
				out = append(out, parent+name)
			}
			switch n := a.(type) {
			case schema.SingleNestedAttribute:
				walk(n.Attributes, parent+name+".", inCollection)
			case schema.ListNestedAttribute:
				walk(n.NestedObject.Attributes, parent+name+".", true)
			case schema.SetNestedAttribute:
				walk(n.NestedObject.Attributes, parent+name+".", true)
			case schema.MapNestedAttribute:
				walk(n.NestedObject.Attributes, parent+name+".", true)
			}
		}
	}
	walk(s.Attributes, "", false)
	slices.Sort(out)
	return out
}

// replacementModifiers returns an attribute's RequiresReplace modifiers, typed
// as whatever modifier kind the attribute takes.
func replacementModifiers(a schema.Attribute) []any {
	var all []any
	switch a := a.(type) {
	case schema.StringAttribute:
		all = anySlice(a.PlanModifiers)
	case schema.Int64Attribute:
		all = anySlice(a.PlanModifiers)
	case schema.BoolAttribute:
		all = anySlice(a.PlanModifiers)
	case schema.ListAttribute:
		all = anySlice(a.PlanModifiers)
	case schema.ListNestedAttribute:
		all = anySlice(a.PlanModifiers)
	case schema.SetAttribute:
		all = anySlice(a.PlanModifiers)
	case schema.SetNestedAttribute:
		all = anySlice(a.PlanModifiers)
	case schema.MapAttribute:
		all = anySlice(a.PlanModifiers)
	case schema.MapNestedAttribute:
		all = anySlice(a.PlanModifiers)
	case schema.ObjectAttribute:
		all = anySlice(a.PlanModifiers)
	case schema.SingleNestedAttribute:
		all = anySlice(a.PlanModifiers)
	}
	return slices.DeleteFunc(all, func(m any) bool { return !IsReplacementModifier(m) })
}

func anySlice[M any](mods []M) []any {
	out := make([]any, len(mods))
	for i, m := range mods {
		out[i] = m
	}
	return out
}

// IsReplacementModifier reports whether a plan modifier is one of the
// framework's RequiresReplace family. Every replacement in this provider's
// schemas is built from RequiresReplace or RequiresReplaceIf, which share one
// unexported type per attribute kind. The name is the framework's, so
// TestReplacementModifiersAreRecognised pins it: an upgrade that renames the
// type fails that test rather than quietly unguarding every resource.
func IsReplacementModifier(m any) bool {
	t := reflect.TypeOf(m)
	if t == nil {
		return false
	}
	return t.Name() == "requiresReplaceIfModifier" &&
		strings.HasPrefix(t.PkgPath(), "github.com/hashicorp/terraform-plugin-framework/resource/schema/")
}

// replacementAllowed reads the opt-in from the configuration being planned. A
// null or unknown value is a refusal: an unknown cannot be shown to be true.
func replacementAllowed(ctx context.Context, config tfsdk.Config) bool {
	var allow types.Bool
	if d := config.GetAttribute(ctx, path.Root(allowReplaceAttribute), &allow); d.HasError() {
		return false
	}
	return allow.ValueBool()
}

// guardReplacement refuses every replacement in this plan unless the
// configuration permits it. The guarded resource's ModifyPlan calls it after
// the resource's own, so region and project are already resolved and the plan
// is final.
//
// It runs on a plan the resource has already failed, too, so that the refusal
// is reported with the resource's own errors rather than only once they're
// fixed.
func guardReplacement(ctx context.Context, what tenantState, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || resp.Plan.Raw.IsNull() {
		return // a create or a destroy
	}
	if replacementAllowed(ctx, req.Config) {
		return
	}
	replacing, ok := plannedReplacements(ctx, req, resp)
	if !ok {
		// Local: fails closed. Unreachable while every resource is built from
		// resource/schema, and a plan that cannot be checked is not let through.
		resp.Diagnostics.AddError("The replacement guard could not read this resource's schema",
			fmt.Sprintf("The plan was refused because the provider cannot tell whether it replaces this %s.", what.noun))
		return
	}
	for _, r := range replacing {
		refuseReplacement(&resp.Diagnostics, r.path, r.from, r.to, what)
	}
}

// plannedReplacement is one attribute whose planned change replaces the
// resource.
type plannedReplacement struct {
	path     path.Path
	from, to attr.Value
}

// plannedReplacements lists every attribute whose change in this plan replaces
// the resource, the way Terraform will decide it: region and project from
// ModifyPlan's own response, and every other attribute by asking its
// replacement modifiers again against its final planned value. ok is false
// when the schema can't be read, so a caller can fail closed. A create or a
// destroy replaces nothing.
func plannedReplacements(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) (out []plannedReplacement, ok bool) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return nil, true
	}
	s, ok := req.Plan.Schema.(schema.Schema)
	if !ok {
		return nil, false
	}

	// ModifyPlan's own decisions: a moved region or project.
	for _, p := range resp.RequiresReplace {
		// Any type: today only region and project arrive here, but a
		// replacement a resource plans on a number or a block is refused
		// by name too.
		var prior, planned attr.Value
		resp.Diagnostics.Append(req.State.GetAttribute(ctx, p, &prior)...)
		resp.Diagnostics.Append(resp.Plan.GetAttribute(ctx, p, &planned)...)
		out = append(out, plannedReplacement{path: p, from: prior, to: planned})
	}

	in := probeInput{ctx: ctx, req: req, plan: resp.Plan}
	var walk func(attrs map[string]schema.Attribute, parent path.Path)
	walk = func(attrs map[string]schema.Attribute, parent path.Path) {
		for _, name := range sortedKeys(attrs) {
			p := parent.AtName(name)
			if from, to, replaces := in.replaces(attrs[name], p); replaces {
				out = append(out, plannedReplacement{path: p, from: from, to: to})
				continue
			}
			if nested, ok := attrs[name].(schema.SingleNestedAttribute); ok {
				walk(nested.Attributes, p)
			}
		}
	}
	walk(s.Attributes, path.Empty())
	return out, true
}

// refuseReplacement is the one message every refusal uses.
func refuseReplacement(diags *diag.Diagnostics, p path.Path, from, to attr.Value, what tenantState) {
	name := p.String()
	// Local: decided from configuration and prior state, no request made.
	diags.AddAttributeError(p,
		fmt.Sprintf("Changing `%s` would replace this %s", name, what.noun),
		fmt.Sprintf("`%s` goes from %s to %s. That cannot be done in place, so Terraform would "+
			"destroy this %s and create another. %s\n\n"+
			"The plan is refused rather than making the replacement silently. To replace the %s "+
			"on purpose, add `%s = true` to this resource and plan again. To keep it, put `%s` back "+
			"to its current value.\n\n"+
			"If you ran `terraform destroy`: Terraform plans a refresh before every destroy as if "+
			"applying, and this refusal stops that plan. Run `terraform destroy -refresh=false` to "+
			"destroy from state as it is.",
			name, describeValue(from), describeValue(to), what.noun, what.loses,
			what.noun, allowReplaceAttribute, name))
}

func describeValue(v attr.Value) string {
	switch {
	case v == nil || v.IsNull():
		return "nothing"
	case v.IsUnknown():
		return "a value known only after apply"
	}
	return v.String()
}

// probeInput is what asking an attribute's modifiers again needs: the
// request, and the plan as it stands once every modifier has run.
type probeInput struct {
	ctx  context.Context
	req  resource.ModifyPlanRequest
	plan tfsdk.Plan
}

// values reads one attribute from configuration, the final plan and prior
// state.
func values[V any](in probeInput, p path.Path, config, plan, state *V) bool {
	var diags diag.Diagnostics
	diags.Append(in.req.Config.GetAttribute(in.ctx, p, config)...)
	diags.Append(in.plan.GetAttribute(in.ctx, p, plan)...)
	diags.Append(in.req.State.GetAttribute(in.ctx, p, state)...)
	return !diags.HasError()
}

// replaces runs an attribute's replacement modifiers against its final planned
// value, each into a throwaway response, and reports whether one still asks
// for a replacement. One case per modifier kind, because the framework's
// modifier interfaces differ only in their names.
func (in probeInput) replaces(a schema.Attribute, p path.Path) (from, to attr.Value, replaces bool) {
	mods := replacementModifiers(a)
	if len(mods) == 0 {
		return nil, nil, false
	}
	r := in.req
	for _, m := range mods {
		switch m := m.(type) {
		case planmodifier.String:
			var c, pl, st types.String
			if values(in, p, &c, &pl, &st) {
				out := planmodifier.StringResponse{PlanValue: pl}
				m.PlanModifyString(in.ctx, planmodifier.StringRequest{Path: p, PathExpression: p.Expression(),
					Config: r.Config, ConfigValue: c, Plan: in.plan, PlanValue: pl, State: r.State, StateValue: st,
					Private: r.Private}, &out)
				if out.RequiresReplace {
					return st, pl, true
				}
			}
		case planmodifier.Int64:
			var c, pl, st types.Int64
			if values(in, p, &c, &pl, &st) {
				out := planmodifier.Int64Response{PlanValue: pl}
				m.PlanModifyInt64(in.ctx, planmodifier.Int64Request{Path: p, PathExpression: p.Expression(),
					Config: r.Config, ConfigValue: c, Plan: in.plan, PlanValue: pl, State: r.State, StateValue: st,
					Private: r.Private}, &out)
				if out.RequiresReplace {
					return st, pl, true
				}
			}
		case planmodifier.Bool:
			var c, pl, st types.Bool
			if values(in, p, &c, &pl, &st) {
				out := planmodifier.BoolResponse{PlanValue: pl}
				m.PlanModifyBool(in.ctx, planmodifier.BoolRequest{Path: p, PathExpression: p.Expression(),
					Config: r.Config, ConfigValue: c, Plan: in.plan, PlanValue: pl, State: r.State, StateValue: st,
					Private: r.Private}, &out)
				if out.RequiresReplace {
					return st, pl, true
				}
			}
		case planmodifier.List:
			var c, pl, st types.List
			if values(in, p, &c, &pl, &st) {
				out := planmodifier.ListResponse{PlanValue: pl}
				m.PlanModifyList(in.ctx, planmodifier.ListRequest{Path: p, PathExpression: p.Expression(),
					Config: r.Config, ConfigValue: c, Plan: in.plan, PlanValue: pl, State: r.State, StateValue: st,
					Private: r.Private}, &out)
				if out.RequiresReplace {
					return st, pl, true
				}
			}
		case planmodifier.Set:
			var c, pl, st types.Set
			if values(in, p, &c, &pl, &st) {
				out := planmodifier.SetResponse{PlanValue: pl}
				m.PlanModifySet(in.ctx, planmodifier.SetRequest{Path: p, PathExpression: p.Expression(),
					Config: r.Config, ConfigValue: c, Plan: in.plan, PlanValue: pl, State: r.State, StateValue: st,
					Private: r.Private}, &out)
				if out.RequiresReplace {
					return st, pl, true
				}
			}
		case planmodifier.Map:
			var c, pl, st types.Map
			if values(in, p, &c, &pl, &st) {
				out := planmodifier.MapResponse{PlanValue: pl}
				m.PlanModifyMap(in.ctx, planmodifier.MapRequest{Path: p, PathExpression: p.Expression(),
					Config: r.Config, ConfigValue: c, Plan: in.plan, PlanValue: pl, State: r.State, StateValue: st,
					Private: r.Private}, &out)
				if out.RequiresReplace {
					return st, pl, true
				}
			}
		case planmodifier.Object:
			var c, pl, st types.Object
			if values(in, p, &c, &pl, &st) {
				out := planmodifier.ObjectResponse{PlanValue: pl}
				m.PlanModifyObject(in.ctx, planmodifier.ObjectRequest{Path: p, PathExpression: p.Expression(),
					Config: r.Config, ConfigValue: c, Plan: in.plan, PlanValue: pl, State: r.State, StateValue: st,
					Private: r.Private}, &out)
				if out.RequiresReplace {
					return st, pl, true
				}
			}
		}
	}
	return nil, nil, false
}
