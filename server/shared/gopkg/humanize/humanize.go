// Package humanize writes numbers in a form meant for people to read.
//
// # This package does one thing only: byte count -> human-readable string
//
// The name is its whole scope. **Do not add anything else**: it is not "anything about formatting
// can go here",
// it is the single job of "writing a quantity for people". Test: the object in the function name
// must be a quantity.
//
// This has to be stated firmly, because the package used to be one named util. Names like util
// naturally turn into a junk drawer:
// anyone can drop things in, a wrong placement causes no error, and the next person just follows
// along. The work going on in this repo
// (splitting up the very large core package) is paying off exactly that kind of debt.
// The repo's naming rule is in the first paragraph of the conventions section of the contributor
// guide: a package name appears at every call site,
// so the cost of a bad choice grows with the number of references.
//
// # Why it deserves its own package
//
// HumanBytes is used by 6 files across four resource domains: skill / issue / suggestion /
// devblob.
// core is being split into packages by domain. If cross-domain code is not pulled out first, the first
// domain to move takes it along,
// and the other domains stop compiling.
//
// ⚠️ Two other functions from the same round were **not** moved here, because they are not
// cross-domain:
// `isTransient` is used only by core/integration*.go (one domain),
// `filterSlice` has only 3 call sites; both stay in core.
// "Move only if cross-domain" is the test for this step, not "move it because it is a utility".
package humanize

import "fmt"

// HumanBytes formats a byte count in a form meant for people to read.
//
// It only goes up to MiB: every caller's upper limit is within that range.
func HumanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d 字节", n)
	}
}
