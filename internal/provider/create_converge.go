package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A create that is safe to send twice.
//
// A repeated run after an interrupted one has to converge rather than
// duplicate. The platform makes a create safe to repeat only when it
// names its id: a second create with the same id answers 409 and makes
// nothing, while a create with no id makes a new resource every time. No
// Idempotency-Key support was found. So every create this provider sends names
// an id, generating one when the configuration sets none.
//
// # Why the id is generated in Create, not in the plan
//
// A random id set in ModifyPlan would show in the plan, but Terraform plans a
// create a second time during apply and refuses a known planned value that
// changed between the two ("Provider produced inconsistent final plan"). The
// provider is handed nothing that survives from one to the other, so a plan
// time id would have to be derived from configuration, and two resources with
// the same configuration would then share it. The id is therefore made in
// Create, the way the random provider makes its values, and `id` stays
// "known after apply".
//
// Terraform takes no state from a provider mid-call, so "recorded before the
// create is sent" comes down to this: from the moment the request leaves,
// every way out of Create carries the id. When the answer is lost and a read
// cannot settle whether the create happened, Create records the id and fails.
// Terraform keeps that row as tainted, so the next run refreshes it and either
// drops it (it was never made) or replaces it, unless `terraform untaint`
// keeps it. Neither run makes a second one.
//
// Only a generated id is recorded that way. A configured id may already name
// something outside this state, and a tainted row pointing at it would let the
// next run refresh it and plan to destroy it. So with a configured id an
// unsettled create fails with nothing in state, and if it did land, the next
// run's create answers 409 and stops with the import command.
//
// What this cannot cover is a provider process that dies between sending the
// create and returning, since nothing reaches state then. With a configured id
// the next run's create answers 409, which carries no token from that run, so
// it stops with the import command. With a generated id the next run
// generates another, and a second resource is made.
//
// # Proof of ownership
//
// A 409 on a create says something with that id exists, not who made it. The
// provider adopts it only when it can show this Create call made it:
//
//   - A kind with user tags carries a random create token under
//     createTokenTag. A read whose token matches is this call's resource.
//   - A kind without tags has only its id. A generated id is 16 random hex
//     digits made in this call, so finding it is the same proof. A configured
//     id proves nothing, and a 409 on one is reported with the import command.
//
// Nothing is adopted across runs: a token lives only as long as one Create.

// createTokenTagSuffix follows the provider's name in the token's tag key.
const createTokenTagSuffix = "/create-token"

// createTokenTag is the tag a create token travels in. It is built from
// ProviderTypeName so a rename reaches it.
func createTokenTag() string {
	return "terraform-provider-" + ProviderTypeName + createTokenTagSuffix
}

// generatedIDPrefix starts every id this provider makes up. The API's pattern
// is `[a-z]([-a-z0-9]*[a-z0-9])?`, 1 to 63 characters: the prefix supplies the
// leading letter and the hex digits end on a letter or digit.
const generatedIDPrefix = "tf-"

// createAttempts bounds how many times one Create sends its request after a
// lost answer that a read showed did not land. A resend after a refusal a
// busy dependency explains is bounded by the create's timeout instead, since
// each one waits for that dependency first.
const createAttempts = 3

// createKey is what one Create call sends to make its create safe to repeat.
type createKey struct {
	id        string
	generated bool
	// token is empty for a kind that has no tags to carry one.
	token string
}

// newCreateKey takes the configured id, or makes one, and a token when the
// kind can carry it.
func newCreateKey(configured types.String, carriesToken bool) (createKey, error) {
	key := createKey{id: configured.ValueString()}
	if configured.IsNull() || configured.IsUnknown() || key.id == "" {
		suffix, err := randomHex(8)
		if err != nil {
			return createKey{}, err
		}
		key.id, key.generated = generatedIDPrefix+suffix, true
	}
	if carriesToken {
		token, err := randomHex(16)
		if err != nil {
			return createKey{}, err
		}
		key.token = token
	}
	return key, nil
}

// addCreateKeyError reports that no key could be made. Nothing has been sent.
func addCreateKeyError(diags *diag.Diagnostics, kind string, err error) {
	// Local: no request was made.
	diags.AddError(
		fmt.Sprintf("Could not name the new %s", kind),
		fmt.Sprintf("The provider makes an id and a create token from the system's random source "+
			"before it sends a create, and reading it failed: %s. Nothing was sent.", err),
	)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// tagged returns the user's tags with the token added, leaving theirs alone.
// With no token it returns them unchanged.
func (k createKey) tagged(tags *map[string]string) *map[string]string {
	if k.token == "" {
		return tags
	}
	out := map[string]string{createTokenTag(): k.token}
	if tags != nil {
		for name, v := range *tags {
			out[name] = v
		}
	}
	return &out
}

// taggedFreeform is tagged for the security group's freeform tags.
func (k createKey) taggedFreeform(tags *map[string]any) *map[string]any {
	if k.token == "" {
		return tags
	}
	out := map[string]any{createTokenTag(): k.token}
	if tags != nil {
		for name, v := range *tags {
			out[name] = v
		}
	}
	return &out
}

// createTokenOf reads the create token out of a read's tags.
func createTokenOf(tags *map[string]string) string {
	if tags == nil {
		return ""
	}
	return (*tags)[createTokenTag()]
}

// keepCreateToken is tags with the create token the platform holds put back.
// A PATCH's tags replace the platform's, and the user's tags never carry the
// token, so a tag change sent as it is drops it. A cluster without its token
// reads as made outside Terraform, which refuses the plan that destroys it
// once it's tainted. With no token held, tags come back unchanged.
func keepCreateToken(tags *map[string]string, token string) *map[string]string {
	if tags == nil || token == "" {
		return tags
	}
	return createKey{token: token}.tagged(tags)
}

// createTokenOfFreeform is createTokenOf for freeform tags.
func createTokenOfFreeform(tags *map[string]any) string {
	if tags == nil {
		return ""
	}
	if s, ok := (*tags)[createTokenTag()].(string); ok {
		return s
	}
	return ""
}

// createAnswer is what one create request came back with.
type createAnswer struct {
	// id is the id a 2xx answer named, empty when it named none. It is
	// compared with the id sent, never adopted in its place.
	id string
	// uid is the uid a 2xx answer named, empty when it named none.
	uid  string
	resp *http.Response
	body []byte
	err  error
}

// createLookup is what reading an id back found.
type createLookup struct {
	found bool
	token string
	// uid is the uid of what was found, empty when it named none.
	uid string
	// resp, body and err describe a read that failed. The service's 404 is not
	// a failure: it is found == false with all three empty.
	resp *http.Response
	body []byte
	err  error
}

func (l createLookup) failed() bool { return l.err != nil || l.resp != nil }

// createOutcome is how a converging create ended.
type createOutcome int

const (
	// createFailed: nothing to record. The diagnostics say why.
	createFailed createOutcome = iota
	// createDone: the id names a resource this Create made. Carry on with it.
	createDone
	// createUnconfirmed: the answer was lost and a read could not say whether
	// the create landed. Record the id in state, so the next run refreshes it
	// rather than creating again, and fail.
	createUnconfirmed
)

// createdAs is the object a converging create ended on.
type createdAs struct {
	id string
	// uid is the platform's uid for it, empty when no answer or read named
	// one.
	uid string
}

// convergingCreate is one Create's create request, with what it needs to
// recover from a lost answer or a 409.
type convergingCreate struct {
	key createKey
	// kind is the resource as a sentence names it, e.g. "cluster".
	kind string
	// what names the call for addAPIError, e.g. "create the cluster".
	what apiCall
	// resourceName is the Terraform type, for the import command.
	resourceName string
	// importID turns an id into the form `terraform import` takes for it.
	importID func(id string) string
	// send makes the create request.
	send func(ctx context.Context) createAnswer
	// lookup reads an id back.
	lookup func(ctx context.Context, id string) createLookup
	// timeoutID is the id a 504 names in its metadata, if it names one. Nil
	// for a kind whose 504 names none.
	timeoutID func(resp *http.Response, body []byte) string
	// busy waits out a 409 the platform answers while something the create
	// depends on is mid-change. Nil for a kind whose create has no such 409.
	busy *busyRetry
}

// run sends the create until it has an answer it can act on.
//
//   - 2xx: done, with the id the create sent and the uid the answer named. An
//     answer naming another id is refused.
//   - No answer at all: read the id back. Found and provably this call's:
//     done. Found and not provable: refused. Not found: send again, up to
//     createAttempts. The read failing too: unconfirmed, for a generated id.
//   - 409: read the id back. Found and provably this call's: done. Otherwise,
//     when busy says something the create depends on was mid-change and has
//     settled, send again, since nothing was made for this call under the
//     id. Otherwise the 409 is reported, with the import command when the id
//     exists.
//   - 504 naming an id in its metadata: the service persisted that resource
//     for this request and timed out starting its workflow. Done, with that id
//     when it is the one sent, and refused when it isn't. Its uid is read back.
//   - Any other 502 or 504: the request may have been carried out behind the
//     gateway. Read the id back. Found and provably this call's: done. Not
//     found: the error is reported. The read failing: unconfirmed, for a
//     generated id.
//   - Anything else: reported as the platform answered it.
//
// A resource found by reading it back is done with the uid that read named.
func (c convergingCreate) run(ctx context.Context) (createOutcome, createdAs, diag.Diagnostics) {
	var diags diag.Diagnostics
	lost := 0
	for {
		a := c.send(ctx)
		if a.err == nil && statusOf(a.resp)/100 == 2 {
			// The platform names the resource by the id it was sent, so an
			// answer that leaves it out still says which one it made, and
			// one that names another is refused rather than believed.
			if a.id != "" && a.id != c.key.id {
				c.addNamedAnother(&diags, statusOf(a.resp), a.id)
				return createFailed, createdAs{}, diags
			}
			return createDone, createdAs{id: c.key.id, uid: a.uid}, diags
		}

		switch status := statusOf(a.resp); {
		case a.err != nil:
			if ctx.Err() != nil {
				c.addUnconfirmed(&diags, a.err, nil)
				return c.unconfirmed(diags)
			}
			l := c.lookup(ctx, c.key.id)
			if l.failed() {
				c.addUnconfirmed(&diags, a.err, &l)
				return c.unconfirmed(diags)
			}
			if l.found {
				return c.claim(&diags, c.key.id, l, "lost its answer", a)
			}
			lost++
			if lost >= createAttempts {
				addAPIError(&diags, c.what, nil, nil, a.err)
				return createFailed, createdAs{}, diags
			}
			tflog.Info(ctx, "the create's answer was lost and the id reads as absent, so sending it again",
				map[string]any{"id": c.key.id, "attempt": lost + 1, "error": redactDiagnosticText(a.err.Error())})

		case status == http.StatusConflict:
			l := c.lookup(ctx, c.key.id)
			if l.found && c.proof(c.key.id, l) != "" {
				return c.claim(&diags, c.key.id, l, "answered 409", a)
			}
			// Something under the id that this call didn't make may be on its
			// way out, like a security group still tearing down, so busy reads
			// it before the 409 is reported as a name in use.
			if !l.failed() {
				again, waitErr := c.busy.afterConflict(ctx, a.resp, a.body)
				if again {
					continue
				}
				if waitErr != nil {
					c.busy.report(&diags, c.what, a.resp, a.body, waitErr)
					return createFailed, createdAs{}, diags
				}
			}
			if l.found {
				return c.claim(&diags, c.key.id, l, "answered 409", a)
			}
			addAPIError(&diags, c.what, a.resp, a.body, nil)
			if l.failed() {
				addLookupError(&diags, c.kind, c.key.id, l)
			}
			return createFailed, createdAs{}, diags

		case status == http.StatusBadGateway || status == http.StatusGatewayTimeout:
			if named := c.namedOnTimeout(a); named != "" {
				if named != c.key.id {
					c.addNamedAnother(&diags, status, named)
					return createFailed, createdAs{}, diags
				}
				// The service's own answer to this request: it persisted the
				// resource and timed out starting its workflow. That is proof
				// enough, so the resource is waited out like a 201.
				diags.AddWarning(
					fmt.Sprintf("The %s was created, and its workflow was slow to start", c.kind),
					fmt.Sprintf("The platform answered %d and named %s %q as created, so this apply "+
						"waits for it to settle rather than sending the create again.", status, c.kind, named),
				)
				// A 504 carries no uid, so it's read back now rather than on
				// the first read after the wait, which may come too late.
				made := createdAs{id: named}
				if l := c.lookup(ctx, named); l.found {
					made.uid = l.uid
				}
				return createDone, made, diags
			}
			l := c.lookup(ctx, c.key.id)
			if l.found {
				return c.claim(&diags, c.key.id, l, fmt.Sprintf("answered %d", status), a)
			}
			addAPIError(&diags, c.what, a.resp, a.body, nil)
			if l.failed() {
				addLookupError(&diags, c.kind, c.key.id, l)
				return c.unconfirmed(diags)
			}
			return createFailed, createdAs{}, diags

		default:
			addAPIError(&diags, c.what, a.resp, a.body, nil)
			return createFailed, createdAs{}, diags
		}
	}
}

// unconfirmed ends a create that a read could not settle. A generated id is
// returned to be recorded: it was made in this call, so whatever it names is
// this call's. A configured id is not, since it may name something this state
// has never managed.
func (c convergingCreate) unconfirmed(diags diag.Diagnostics) (createOutcome, createdAs, diag.Diagnostics) {
	if !c.key.generated {
		return createFailed, createdAs{}, diags
	}
	return createUnconfirmed, createdAs{id: c.key.id}, diags
}

// namedOnTimeout is the id a 504 from the service names, if any. Only a 504
// carries one: the gateway's own timeouts do not.
func (c convergingCreate) namedOnTimeout(a createAnswer) string {
	if c.timeoutID == nil || statusOf(a.resp) != http.StatusGatewayTimeout {
		return ""
	}
	return c.timeoutID(a.resp, a.body)
}

// addNamedAnother refuses an answer that names a resource by an id this create
// did not send.
//
// Every create names its id, and the platform makes the resource under the id
// it was sent, so a 2xx or a 504 naming another one is not an answer to this
// request in any way the provider can account for. Adopting it would put an id
// in state that this apply never chose, and a later destroy would delete
// whatever that id names. Nothing is recorded: the id this create sent may
// still read back, and the one the answer named may not be this
// configuration's at all, so both are left for the operator to look at.
func (c convergingCreate) addNamedAnother(diags *diag.Diagnostics, status int, named string) {
	diags.AddError(
		fmt.Sprintf("The platform named another %s than the one this create sent", c.kind),
		redactDiagnosticText(fmt.Sprintf("The create sent %s %q, and the platform answered %d naming %q. "+
			"The platform makes a resource under the id it is sent, so the provider doesn't take "+
			"the answer at its word, and nothing is recorded in state.\n\n"+
			"Check which of the two exists. If %q is the one this configuration describes, import it, "+
			"replacing <address> with this resource's address, such as %s.example:\n\n"+
			"    terraform import <address> %s",
			c.kind, c.key.id, status, named, named, c.resourceName, c.importID(named))),
	)
}

// claim decides whether a resource found under id is this call's. why says
// what the create did, as in "the create %s", and a is its answer, quoted on a
// refusal.
func (c convergingCreate) claim(diags *diag.Diagnostics, id string, l createLookup, why string, a createAnswer) (createOutcome, createdAs, diag.Diagnostics) {
	proof := c.proof(id, l)
	if proof == "" {
		c.addNotOurs(diags, id, why, a)
		return createFailed, createdAs{}, *diags
	}
	diags.AddWarning(
		fmt.Sprintf("The %s was created, though the create did not say so", c.kind),
		fmt.Sprintf("The create for %s %q %s. Reading it back found it, and %s, so this apply "+
			"carries on with it rather than creating another.", c.kind, id, why, proof),
	)
	return createDone, createdAs{id: id, uid: l.uid}, *diags
}

// proof says how a resource found under id is shown to be this call's, or ""
// when it can't be.
func (c convergingCreate) proof(id string, l createLookup) string {
	switch {
	case c.key.token != "":
		if l.token == c.key.token {
			return "its create token matches the one this apply sent"
		}
	case c.key.generated && id == c.key.id:
		return "its id is the random one this apply generated"
	}
	return ""
}

// addNotOurs refuses a resource this call cannot show it made, quoting what
// the platform answered when it answered.
func (c convergingCreate) addNotOurs(diags *diag.Diagnostics, id, why string, a createAnswer) {
	cannot := "it carries no create token from this apply"
	if c.key.token == "" {
		cannot = fmt.Sprintf("a %s has no tags to carry a create token, and its id was set in configuration", c.kind)
	}
	detail := fmt.Sprintf("The create for %s %q %s. Reading it back found one this apply cannot "+
		"show it created, since %s, so it is not taken over.\n\n"+
		"If it is the one this configuration describes, import it, replacing <address> with this "+
		"resource's address, such as %s.example:\n\n"+
		"    terraform import <address> %s\n\n"+
		"Otherwise choose another `id`.",
		c.kind, id, why, cannot, c.resourceName, c.importID(id))
	var apiErr *k0rdent.APIError
	if a.resp != nil && errors.As(k0rdent.CheckResponse(a.resp, a.body), &apiErr) {
		detail += "\n\n" + apiErrorDetail(apiErr)
	}
	diags.AddError(fmt.Sprintf("%s named %q already exists", aKind(c.kind), id), redactDiagnosticText(detail))
}

// recordUnconfirmed writes the plan to state under id, with every value the
// plan left unknown as null, for a create whose answer was lost and whose read
// failed. Terraform keeps the row tainted. Keeping what the configuration
// asked for, rather than the id alone, means the refresh that follows reads
// the resource back against it, the way the create's own read would have, so
// a row that is untainted plans clean rather than as an import would.
func recordUnconfirmed(ctx context.Context, plan tfsdk.Plan, state *tfsdk.State, id string) diag.Diagnostics {
	diags := recordPlan(plan, state)
	if diags.HasError() {
		return diags
	}
	diags.Append(state.SetAttribute(ctx, path.Root("id"), id)...)
	return diags
}

// recordPlan writes the plan to state with every unknown value as null. Every
// value the plan knew is kept, which is what Terraform holds a create that
// succeeds to, so it also serves a create that returns while the platform is
// still building.
func recordPlan(plan tfsdk.Plan, state *tfsdk.State) diag.Diagnostics {
	var diags diag.Diagnostics
	raw, err := tftypes.Transform(plan.Raw, func(_ *tftypes.AttributePath, v tftypes.Value) (tftypes.Value, error) {
		if !v.IsKnown() {
			return tftypes.NewValue(v.Type(), nil), nil
		}
		return v, nil
	})
	if err != nil {
		// Local: the provider could not copy its own plan.
		diags.AddError("Could not record the create", err.Error())
		return diags
	}
	state.Raw = raw
	return diags
}

// addUnconfirmed reports a lost answer that a read could not settle.
func (c convergingCreate) addUnconfirmed(diags *diag.Diagnostics, sendErr error, l *createLookup) {
	detail := fmt.Sprintf("The create for %s %q got no answer: %s", c.kind, c.key.id, sendErr)
	if l != nil {
		detail += fmt.Sprintf("\n\nReading %q back to see whether it landed failed too: %s",
			c.key.id, lookupErrorText(*l))
	}
	if c.key.generated {
		detail += fmt.Sprintf("\n\nState records %q so that the next run reads it back rather than "+
			"creating another. Terraform marks it tainted: if the next plan finds it and proposes to "+
			"replace it, `terraform untaint` keeps it as it is.", c.key.id)
	} else {
		detail += fmt.Sprintf("\n\nNothing is recorded in state, because %q was set in configuration "+
			"and may name a %s this state does not manage. If the create did land, the next run's "+
			"create answers 409 and gives the import command.", c.key.id, c.kind)
	}
	diags.AddError(fmt.Sprintf("Could not confirm the %s was created", c.kind), redactDiagnosticText(detail))
}

// addLookupError reports a read-back that failed.
func addLookupError(diags *diag.Diagnostics, kind, id string, l createLookup) {
	diags.AddError(
		fmt.Sprintf("Could not read the %s back after its create", kind),
		redactDiagnosticText(fmt.Sprintf("Reading %q back failed: %s", id, lookupErrorText(l))),
	)
}

func lookupErrorText(l createLookup) string {
	if l.err != nil {
		return l.err.Error()
	}
	err := k0rdent.CheckResponse(l.resp, l.body)
	var apiErr *k0rdent.APIError
	if errors.As(err, &apiErr) {
		return apiErrorDetail(apiErr)
	}
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("the platform answered %s", l.resp.Status) + requestIDLine(k0rdent.RequestIDFromHeader(l.resp.Header))
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// lookupFrom turns a generated GET that answered into a createLookup. ok says
// the body decoded, and the token and the uid are read from it only then.
func lookupFrom(resp *http.Response, body []byte, ok bool, found func() (token, uid string)) createLookup {
	switch {
	case isNotFound(resp, body):
		return createLookup{}
	case !ok:
		return createLookup{resp: resp, body: body}
	default:
		token, uid := found()
		return createLookup{found: true, token: token, uid: uid}
	}
}

// timeoutMetadataID reads the id a 504's error details name under key.
func timeoutMetadataID(key string) func(*http.Response, []byte) string {
	return func(resp *http.Response, body []byte) string {
		var apiErr *k0rdent.APIError
		if errors.As(k0rdent.CheckResponse(resp, body), &apiErr) {
			for _, d := range apiErr.Details {
				if meta, ok := d["metadata"].(map[string]any); ok {
					if id, ok := meta[key].(string); ok && id != "" {
						return id
					}
				}
			}
		}
		return ""
	}
}

// scopedImportID is the import form for a project-scoped kind.
func scopedImportID(target clusterTarget) func(string) string {
	return func(id string) string { return target.region + "/" + target.project + "/" + id }
}

// nestedScopedImportID is the import form for a kind under a parent.
func nestedScopedImportID(target clusterTarget, parent string) func(string) string {
	return func(id string) string {
		return target.region + "/" + target.project + "/" + parent + "/" + id
	}
}

// createFinish is a Create from its converging create to its read back: run
// the create, put the id it ended on in state, wait for the resource to
// settle, and read it back.
//
// The plan goes to state under the id before the wait, with the scope and any
// parent it lives under, and so does the uid the create's answer or read
// named, so the row stays bound to this object if the wait fails. If the wait
// or the read back fails, the resource exists on the platform and that row is
// what keeps it from being orphaned:
// Terraform keeps it tainted, and a later apply can finish it or destroy it.
// Nothing is ever deleted on a failed wait. The row holds the plan, not the id
// alone, for the reason recordUnconfirmed gives: with node_pool null, the
// refresh after `terraform untaint` adopted every pool, and the plan proposed
// dropping the ones the configuration never named.
//
// A wait that stops while the platform is still building, on a timeout or an
// interrupt, says so and gives the untaint that lets the next apply wait for
// the same object. See resume.go.
type createFinish[M any] struct {
	// operation is the create's operationId, e.g. "createNetwork", which a
	// read back that answers 404 names.
	operation string
	target    clusterTarget
	// parentAttr and parentID name the resource a nested kind lives under,
	// such as a subnet's network. Empty for a project-scoped kind.
	parentAttr, parentID string
	// wait waits for the resource to settle.
	wait func(ctx context.Context, id string) error
	// waitSummary is the error's summary when the wait fails. Empty means
	// "The <kind> did not become active", with the converging create's kind.
	waitSummary string
	// reportWait reports a failed wait. Nil means the plain addWaitError.
	reportWait func(diags *diag.Diagnostics, summary string, err error)
	// read reads the settled resource back. found is false on a 404.
	read func(ctx context.Context, id string) (state M, found bool, diags diag.Diagnostics)
}

// run runs create and finishes it, writing state and private as it goes. It
// returns the read back state, already set, and true once the read back found
// the resource, so a kind can add what it says about a new one. A failed
// create writes nothing. An unconfirmed one writes the plan under its id, as
// recordUnconfirmed says, and stops there. So does one whose wait fails.
func (f createFinish[M]) run(ctx context.Context, create convergingCreate, plan tfsdk.Plan, state *tfsdk.State, private privateWriter, diags *diag.Diagnostics) (M, bool) {
	var none M
	outcome, made, createDiags := create.run(ctx)
	diags.Append(createDiags...)
	if outcome == createFailed {
		return none, false
	}
	id := made.id
	diags.Append(recordUnconfirmed(ctx, plan, state, id)...)
	if f.parentAttr != "" {
		diags.Append(state.SetAttribute(ctx, path.Root(f.parentAttr), f.parentID)...)
	}
	setCreatedScope(ctx, f.target, state, diags)
	diags.Append(recordUID(ctx, private, made.uid)...)
	if outcome == createUnconfirmed {
		return none, false
	}

	if err := f.wait(ctx, id); err != nil {
		if stillRunning(err) {
			addCreateStillRunning(diags, create.kind, create.resourceName, err, true)
			return none, false
		}
		summary := f.waitSummary
		if summary == "" {
			summary = fmt.Sprintf("The %s did not become active", create.kind)
		}
		report := f.reportWait
		if report == nil {
			report = addWaitError
		}
		report(diags, summary, err)
		return none, false
	}

	got, found, readDiags := f.read(ctx, id)
	diags.Append(readDiags...)
	if diags.HasError() {
		return none, false
	}
	if !found {
		// Local: the provider's conclusion from two calls.
		diags.AddError(
			fmt.Sprintf("The new %s could not be read back", create.kind),
			fmt.Sprintf("`%s` reported %q as created but reading it answered 404.", f.operation, id),
		)
		return none, false
	}
	diags.Append(state.Set(ctx, &got)...)
	return got, true
}
