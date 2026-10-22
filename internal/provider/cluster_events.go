package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// What a failed cluster's diagnostic quotes from the runtime event log.
//
// A cluster that ends `failed` carries one `stateMessage`, and that's often
// the last symptom rather than the cause. listClusterRuntimeEvents is the
// platform's caller-safe account of the operation, and its warnings and
// errors are where the cause shows. So a wait that ends with the cluster
// `failed` reads that log and quotes its last few warnings and errors, the
// way `kubectl describe` ends with a resource's events.
//
// The log is read oldest first, so the lines a diagnostic wants are at the
// end of it. The read follows the cursor to the end and keeps the last
// runtimeEventsShown, bounded at runtimeEventPages pages so a log that runs
// on can't hold the apply up. The read is an aid to the diagnostic, never a
// condition of it: one that fails leaves the diagnostic as it would be
// without the log, and says why only in the debug log.
const (
	// runtimeEventsShown is how many lines the diagnostic quotes. Enough to
	// show a cause and the line before it, few enough not to bury the advice
	// that follows them.
	runtimeEventsShown = 5
	// runtimeEventPageSize is the most the contract allows a page.
	runtimeEventPageSize = 500
	// runtimeEventPages bounds the read at 5,000 warnings and errors.
	runtimeEventPages = 10
	// runtimeEventMessageMax bounds one line's message, after escaping.
	runtimeEventMessageMax = 512
)

// withRuntimeEvents is a wait's error with the event log's lines after it.
// It unwraps to the wait's error, so a caller that asks whether the wait
// timed out still gets the answer it got before.
type withRuntimeEvents struct {
	err    error
	quoted string
}

func (e withRuntimeEvents) Error() string { return e.err.Error() + "\n\n" + e.quoted }
func (e withRuntimeEvents) Unwrap() error { return e.err }

// namesExecution adds the operation's execution id to err, a wait's error. The
// platform's own logs are keyed by it, so it's what to quote to whoever runs
// the landscape.
func namesExecution(err error, execution string) error {
	if execution == "" {
		return err
	}
	return withRuntimeEvents{err: err, quoted: "The platform's execution id for this operation is " +
		oneLine(execution) + ". Quote it to whoever runs the landscape."}
}

// explainFailure adds the event log's last warnings and errors to err, the
// error a wait on cluster id ended with while the cluster read `failed`. A
// read of the log that fails leaves err as it is.
//
// The wait may have spent the operation's deadline, so the read is bounded on
// its own, like recordReached's.
func (r *clusterResource) explainFailure(ctx context.Context, target clusterTarget, id string, err error) error {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	quoted, readErr := r.runtimeEvents(readCtx, target, id)
	if readErr != nil {
		tflog.Debug(ctx, "the runtime event log could not be read, so the failure is reported without it",
			map[string]any{"cluster": id, "error": redactDiagnosticText(readErr.Error())})
		return err
	}
	return withRuntimeEvents{err: err, quoted: quoted}
}

// runtimeEvents reads the warnings and errors of cluster id's current or last
// operation and words the last few of them for a diagnostic.
func (r *clusterResource) runtimeEvents(ctx context.Context, target clusterTarget, id string) (string, error) {
	limit := int32(runtimeEventPageSize)
	params := &k0rdent.ListClusterRuntimeEventsParams{
		Limit: &limit,
		// Left out, the operation is the current or last one, which is the
		// one that failed.
		Level: &k0rdent.ClusterRuntimeEventLevelFilter{"warn", "error"},
	}
	var last []k0rdent.ClusterRuntimeEvent
	total := 0
	for range runtimeEventPages {
		got, err := r.meta.Client.ListClusterRuntimeEventsWithResponse(ctx, target.region, target.project, id, params)
		if err != nil {
			return "", err
		}
		if got.JSON200 == nil {
			return "", k0rdent.CheckResponse(got.HTTPResponse, got.Body)
		}
		// `events` is required, and `[]` when there are none. A 200 without
		// it isn't the log, and reading it as an empty one would tell the
		// user the platform recorded nothing wrong.
		if got.JSON200.Events == nil {
			return "", fmt.Errorf("listClusterRuntimeEvents answered %d without `events`", got.StatusCode())
		}
		for _, e := range got.JSON200.Events {
			// The filter is the platform's, and checked again here so a line
			// it let through can't stand in for a warning.
			if e.Level == nil || (*e.Level != "warn" && *e.Level != "error") {
				continue
			}
			total++
			last = append(last, e)
			if len(last) > runtimeEventsShown {
				last = last[1:]
			}
		}
		cursor := got.JSON200.Cursor
		if len(got.JSON200.Events) < runtimeEventPageSize || cursor == nil || *cursor == "" {
			return quoteRuntimeEvents(last, total, false), nil
		}
		params.After = cursor
	}
	return quoteRuntimeEvents(last, total, true), nil
}

// quoteRuntimeEvents words the lines kept from a log of total warnings and
// errors. stopped says the read reached its bound before the log's end.
func quoteRuntimeEvents(last []k0rdent.ClusterRuntimeEvent, total int, stopped bool) string {
	if total == 0 {
		return "The platform's runtime event log records no warning or error for this operation."
	}
	var b strings.Builder
	if stopped {
		fmt.Fprintf(&b, "The platform's runtime event log for this operation runs past the %d warnings and "+
			"errors the provider reads. These are the last %d it read, and later ones may follow:", total, len(last))
	} else {
		fmt.Fprintf(&b, "The platform's runtime event log for this operation ends with these warnings and "+
			"errors, the last %d of %d:", len(last), total)
	}
	for _, e := range last {
		b.WriteString("\n  ")
		b.WriteString(runtimeEventLine(e))
	}
	return b.String()
}

// runtimeEventLine is one line of the log, kept to one line of the
// diagnostic: every control character in what the platform sent, newlines
// included, is shown escaped, so a message can't pass for a line of its own
// or act on the terminal.
func runtimeEventLine(e k0rdent.ClusterRuntimeEvent) string {
	var parts []string
	if e.OccurredAt != nil {
		parts = append(parts, e.OccurredAt.UTC().Format(time.RFC3339))
	}
	if e.Level != nil {
		parts = append(parts, oneLine(string(*e.Level)))
	}
	var where []string
	if e.Component != nil && *e.Component != "" {
		where = append(where, oneLine(string(*e.Component)))
	}
	if e.Stage != nil && *e.Stage != "" {
		where = append(where, oneLine(string(*e.Stage)))
	}
	head := strings.Join(append(parts, strings.Join(where, ", ")), " ")
	message := ""
	if e.Message != nil {
		message = oneLine(*e.Message)
	}
	if len(message) > runtimeEventMessageMax {
		message = strings.ToValidUTF8(message[:runtimeEventMessageMax], "") + "… (truncated)"
	}
	return strings.TrimSpace(head) + ": " + message
}

// oneLine is escapeControl without its exception for newline and tab.
func oneLine(text string) string {
	escaped := func(r rune) bool { return unicode.IsControl(r) || isEscapedControl(r) }
	if strings.IndexFunc(text, escaped) < 0 {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		if escaped(r) {
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// eventFollower reads the runtime event log as a wait polls, so a create that
// takes an hour isn't an hour of "Still creating..." alone. Each new line goes
// to the provider's INFO log, and a wait that times out names the last one.
// It's best effort: a read that fails stops it, and the wait goes on.
type eventFollower struct {
	r      *clusterResource
	target clusterTarget
	id     string
	after  *string
	last   *k0rdent.ClusterRuntimeEvent
	off    bool
}

// poll reads one page of lines recorded since the last poll.
func (f *eventFollower) poll(ctx context.Context) {
	if f.off {
		return
	}
	limit := int32(runtimeEventPageSize)
	got, err := f.r.meta.Client.ListClusterRuntimeEventsWithResponse(ctx, f.target.region, f.target.project, f.id,
		&k0rdent.ListClusterRuntimeEventsParams{Limit: &limit, After: f.after})
	if err == nil && got.JSON200 == nil {
		err = k0rdent.CheckResponse(got.HTTPResponse, got.Body)
	}
	if err != nil {
		f.off = true
		tflog.Debug(ctx, "the runtime event log could not be read, so the wait goes on without it",
			map[string]any{"cluster": f.id, "error": redactDiagnosticText(err.Error())})
		return
	}
	for i := range got.JSON200.Events {
		e := got.JSON200.Events[i]
		tflog.Info(ctx, "k0rdent: "+redactDiagnosticText(runtimeEventLine(e)), map[string]any{"cluster": f.id})
		f.last = &e
	}
	// An empty page carries no cursor, and the next poll keeps the old one.
	if c := got.JSON200.Cursor; c != nil && *c != "" {
		f.after = c
	}
}

// explainTimeout adds the last line the wait saw to err, a timeout.
func (f *eventFollower) explainTimeout(err error) error {
	if f.last == nil {
		return err
	}
	return withRuntimeEvents{err: err, quoted: "The platform's last runtime event was:\n  " + runtimeEventLine(*f.last)}
}
