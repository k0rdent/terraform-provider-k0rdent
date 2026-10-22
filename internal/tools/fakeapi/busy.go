package main

// busyUntilRefused in a display name holds a cluster's create, scale or
// upgrade, or a file store's create, in flight until the platform has refused
// a write against it because it's busy: a delete of the cluster, or an update
// of the store. Then it settles as anything else does, settleAfter reads
// later.
//
// It stands in for an operation another run started, which finishes on its
// own while this run waits. Nothing a client sends makes a real operation
// last exactly as long as a test needs, and a read count alone can't put the
// refusal inside the operation, since the create that starts it waits for it
// too. Tying the end to the refusal makes the order certain: the write is
// refused while the object is busy, and only a provider that then waits and
// sends it again gets anywhere. One that cancels finds nothing that needed
// cancelling. One that reports the refusal fails.
const busyUntilRefused = "fake-busy-until-refused"

// holdsUntilRefused reports whether an object in state, named displayName,
// is still held by busyUntilRefused. refused says a busy write has been
// refused against it.
func holdsUntilRefused(state string, displayName *string, refused bool) bool {
	return (state == "creating" || state == "updating") && !refused && nameCarries(displayName, busyUntilRefused)
}

// busyAtFirstUpgrade in a cluster's display name starts another run's
// operation on the cluster the moment the first upgrade arrives, so that
// upgrade finds the cluster `updating` and is refused. The operation then
// settles as any other does, settleAfter reads later.
//
// The provider waits for the cluster to settle before each upgrade step, so
// the only way an upgrade meets a busy cluster is an operation another run
// starts between that settle and the step. Nothing a test sends can land in
// that gap on cue, so the fake opens it itself.
const busyAtFirstUpgrade = "fake-busy-at-first-upgrade"

// busyForUpgrade starts that operation when c is due one. The caller holds
// s.mu.
func (c *cluster) busyForUpgrade() {
	if c.state != "active" || c.busiedForUpgrade || !nameCarries(c.displayName, busyAtFirstUpgrade) {
		return
	}
	c.busiedForUpgrade = true
	c.state, c.reads = "updating", 0
	c.start("another run's change to the cluster", "applying")
}
