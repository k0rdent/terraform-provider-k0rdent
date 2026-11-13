package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
)

// The replacement guard's refusal, made at apply.
//
// The framework only ever adds to RequiresReplace, so a resource's ModifyPlan
// can't take back what an attribute's RequiresReplace modifier asked for. The
// guard owns the served schema, so it wraps each of those modifiers instead:
// the wrapper asks the real modifier, and drops its answer when the
// configuration being planned doesn't set `allow_replace = true`. Terraform
// then plans an update, so it never calls Delete. guardReplacement warns and
// records the refusal in planned private state, and the guarded Update reads
// it and refuses before it sends any request.
//
// An opt-in known only after apply drops the answer at plan, and Terraform's
// final plan at apply, with the opt-in known, then asks for a replacement.
// Terraform refuses that itself, as an inconsistent final plan, before it
// changes the resource. Planning the replacement instead would let Terraform
// delete the object before anything could refuse.

// refusedReplacementKey carries the refusal from the plan to the Update.
const refusedReplacementKey = "replacement_refused"

// refusedReplacement is one attribute whose change the apply refuses, and the
// sentence that says what the change was.
type refusedReplacement struct {
	Path   []string `json:"path"`
	Change string   `json:"change"`
}

// pathSteps is a path the guard names as attribute names. Every path the
// guard refuses is one: a top-level attribute or one in a single nested
// object (TestNoReplacementTheGuardCannotReach).
func pathSteps(p path.Path) []string {
	var out []string
	for _, s := range p.Steps() {
		name, ok := s.(path.PathStepAttributeName)
		if !ok {
			break
		}
		out = append(out, string(name))
	}
	return out
}

func pathOf(steps []string) path.Path {
	if len(steps) == 0 {
		return path.Empty()
	}
	p := path.Root(steps[0])
	for _, s := range steps[1:] {
		p = p.AtName(s)
	}
	return p
}

// recordRefusal writes the refusal into the plan's private data.
func recordRefusal(ctx context.Context, private privateWriter, refused []refusedReplacement, diags *diag.Diagnostics) {
	raw, err := json.Marshal(refused)
	if err != nil {
		// Local: fails closed. A slice of strings always marshals.
		diags.AddError("The replacement guard could not record its refusal", err.Error())
		return
	}
	// A plan with no private data fails here: the framework's SetKey
	// refuses a nil one, so the plan errors rather than losing the refusal.
	diags.Append(private.SetKey(ctx, refusedReplacementKey, raw)...)
}

// readRefusal is the refusal the plan recorded, if any.
func readRefusal(ctx context.Context, private privateState, diags *diag.Diagnostics) []refusedReplacement {
	raw, d := private.GetKey(ctx, refusedReplacementKey)
	diags.Append(d...)
	if len(raw) == 0 {
		return nil
	}
	var refused []refusedReplacement
	if err := json.Unmarshal(raw, &refused); err != nil || len(refused) == 0 {
		// Local: fails closed. The plan recorded a refusal this can't read,
		// so the update is refused without naming the attribute.
		refused = []refusedReplacement{{Change: "The plan recorded a change that replaces it."}}
	}
	return refused
}

// refuseAtApply is the apply's half of a refusal: the error for each
// attribute the plan warned about.
func refuseAtApply(diags *diag.Diagnostics, refused []refusedReplacement, what tenantState) {
	for _, r := range refused {
		p := pathOf(r.Path)
		name := p.String()
		summary := fmt.Sprintf("Changing `%s` would replace this %s, so the apply refused it", name, what.noun)
		keep := fmt.Sprintf("To keep the %s, set `%s` back to its current value.", what.noun, name)
		if len(r.Path) == 0 {
			summary = fmt.Sprintf("This change would replace this %s, so the apply refused it", what.noun)
			keep = fmt.Sprintf("To keep the %s, undo the change and plan again.", what.noun)
		}
		detail := fmt.Sprintf("%s That can't be done in place: Terraform would destroy this %s and "+
			"create another. %s\n\n"+
			"The plan warned about this. The apply refused before it sent any request, so the %s is "+
			"unchanged and none of its changes in this plan were made. `terraform destroy` isn't "+
			"affected.\n\n"+
			"To replace the %s on purpose, add `%s = true` to this resource and apply again. %s",
			r.Change, what.noun, what.loses, what.noun, what.noun, allowReplaceAttribute, keep)
		// Local: decided from the plan, no request made.
		if len(r.Path) == 0 {
			diags.AddError(summary, detail)
			continue
		}
		diags.AddAttributeError(p, summary, detail)
	}
}

// replacementWrapper is a RequiresReplace modifier the guard wrapped.
type replacementWrapper interface{ wrappedReplacement() any }

// unwrapReplacement is the modifier a wrapper holds, or m itself.
func unwrapReplacement(m any) any {
	if w, ok := m.(replacementWrapper); ok {
		return w.wrappedReplacement()
	}
	return m
}

// One wrapper per modifier kind, because the framework's modifier interfaces
// differ only in their names. Each embeds the modifier it wraps, so its
// descriptions, and so the docs, are the wrapped one's.
type (
	refusedString struct{ planmodifier.String }
	refusedInt64  struct{ planmodifier.Int64 }
	refusedBool   struct{ planmodifier.Bool }
	refusedList   struct{ planmodifier.List }
	refusedSet    struct{ planmodifier.Set }
	refusedMap    struct{ planmodifier.Map }
	refusedObject struct{ planmodifier.Object }
)

func (w refusedString) wrappedReplacement() any { return w.String }
func (w refusedInt64) wrappedReplacement() any  { return w.Int64 }
func (w refusedBool) wrappedReplacement() any   { return w.Bool }
func (w refusedList) wrappedReplacement() any   { return w.List }
func (w refusedSet) wrappedReplacement() any    { return w.Set }
func (w refusedMap) wrappedReplacement() any    { return w.Map }
func (w refusedObject) wrappedReplacement() any { return w.Object }

func (w refusedString) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	w.String.PlanModifyString(ctx, req, resp)
	resp.RequiresReplace = resp.RequiresReplace && replacementAllowed(ctx, req.Config)
}

func (w refusedInt64) PlanModifyInt64(ctx context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	w.Int64.PlanModifyInt64(ctx, req, resp)
	resp.RequiresReplace = resp.RequiresReplace && replacementAllowed(ctx, req.Config)
}

func (w refusedBool) PlanModifyBool(ctx context.Context, req planmodifier.BoolRequest, resp *planmodifier.BoolResponse) {
	w.Bool.PlanModifyBool(ctx, req, resp)
	resp.RequiresReplace = resp.RequiresReplace && replacementAllowed(ctx, req.Config)
}

func (w refusedList) PlanModifyList(ctx context.Context, req planmodifier.ListRequest, resp *planmodifier.ListResponse) {
	w.List.PlanModifyList(ctx, req, resp)
	resp.RequiresReplace = resp.RequiresReplace && replacementAllowed(ctx, req.Config)
}

func (w refusedSet) PlanModifySet(ctx context.Context, req planmodifier.SetRequest, resp *planmodifier.SetResponse) {
	w.Set.PlanModifySet(ctx, req, resp)
	resp.RequiresReplace = resp.RequiresReplace && replacementAllowed(ctx, req.Config)
}

func (w refusedMap) PlanModifyMap(ctx context.Context, req planmodifier.MapRequest, resp *planmodifier.MapResponse) {
	w.Map.PlanModifyMap(ctx, req, resp)
	resp.RequiresReplace = resp.RequiresReplace && replacementAllowed(ctx, req.Config)
}

func (w refusedObject) PlanModifyObject(ctx context.Context, req planmodifier.ObjectRequest, resp *planmodifier.ObjectResponse) {
	w.Object.PlanModifyObject(ctx, req, resp)
	resp.RequiresReplace = resp.RequiresReplace && replacementAllowed(ctx, req.Config)
}

// wrapReplacements is mods with each RequiresReplace modifier wrapped, in a
// slice of its own, so a schema the resource shares isn't changed.
func wrapReplacements[M any](mods []M, wrap func(M) M) []M {
	out := make([]M, len(mods))
	for i, m := range mods {
		out[i] = m
		if _, wrapped := any(m).(replacementWrapper); !wrapped && IsReplacementModifier(m) {
			out[i] = wrap(m)
		}
	}
	return out
}

// deferReplacements wraps every RequiresReplace modifier in a served schema's
// attributes, at the top level and in single nested objects, the places
// replacementModifiers reads. TestEveryServedReplacementIsWrapped walks every
// attribute kind, so one this misses fails there.
func deferReplacements(attrs map[string]schema.Attribute) {
	for name, a := range attrs {
		switch a := a.(type) {
		case schema.StringAttribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.String) planmodifier.String { return refusedString{m} })
			attrs[name] = a
		case schema.Int64Attribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.Int64) planmodifier.Int64 { return refusedInt64{m} })
			attrs[name] = a
		case schema.BoolAttribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.Bool) planmodifier.Bool { return refusedBool{m} })
			attrs[name] = a
		case schema.ListAttribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.List) planmodifier.List { return refusedList{m} })
			attrs[name] = a
		case schema.ListNestedAttribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.List) planmodifier.List { return refusedList{m} })
			attrs[name] = a
		case schema.SetAttribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.Set) planmodifier.Set { return refusedSet{m} })
			attrs[name] = a
		case schema.SetNestedAttribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.Set) planmodifier.Set { return refusedSet{m} })
			attrs[name] = a
		case schema.MapAttribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.Map) planmodifier.Map { return refusedMap{m} })
			attrs[name] = a
		case schema.MapNestedAttribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.Map) planmodifier.Map { return refusedMap{m} })
			attrs[name] = a
		case schema.ObjectAttribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.Object) planmodifier.Object { return refusedObject{m} })
			attrs[name] = a
		case schema.SingleNestedAttribute:
			a.PlanModifiers = wrapReplacements(a.PlanModifiers, func(m planmodifier.Object) planmodifier.Object { return refusedObject{m} })
			nested := make(map[string]schema.Attribute, len(a.Attributes))
			for k, v := range a.Attributes {
				nested[k] = v
			}
			deferReplacements(nested)
			a.Attributes = nested
			attrs[name] = a
		}
	}
}
