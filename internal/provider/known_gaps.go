package provider

// HardcodedValuesLink is where a description sends a reader whose value the
// platform does not let a tenant read. It is relative to a resource or data
// source page, which sit one directory below the guide's.
const HardcodedValuesLink = "../guides/hardcoded-values.md"

// HardcodedValuesIndexLink is the same guide from the provider's index page,
// which sits beside the guides directory.
const HardcodedValuesIndexLink = "guides/hardcoded-values.md"

// knownGap ends a description for an attribute a configuration has to fill
// with a literal, because no read a tenant can make supplies it. The guide
// names the side that has to change, and the repocheck tests fail when an
// attribute it lists does not end this way.
func knownGap(why string) string {
	return " **Known gap.** " + why + " The [hardcoded values](" + HardcodedValuesLink +
		") guide names the side that has to change."
}
