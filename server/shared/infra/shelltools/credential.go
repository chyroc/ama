// Environment variables the platform injects into the executor: names, the source interface,
// and the table of names that "user-defined env may not override".
//
// # Why they are sent again on every execution instead of once when the container is built
//
// The injected values include credentials that expire (the Feishu user token lasts about 2
// hours), while the container lives longer than that and can be paused/stopped and resumed. If
// we put them in once at build time, the token is already invalid in the later part, and the CLI
// in the container silently gets 401 until the container is deleted. The agent only sees
// commands failing, with nothing pointing to expired credentials. So injection always goes
// through ExecSpec.Envs **on each execution** (see enrich.go / injector.go), and values are
// fetched fresh each time.
//
// A side benefit: credentials do not go on the command line (env is not argv; `ps` cannot see
// it and shell history does not record it).
//
// # Credentials are visible to the agent
//
// The injected token is **fully visible** to bash in the container; one `env` gets the plain
// text. Whatever the CLI can read, bash can read too; this cannot be hidden. What we can do is
// two things: scrub it on the way out (scrub.go, so it does not flow into RunEvent through tool
// results), and tell the user about this on the authorization page.
package shelltools

import (
	"context"
	"strings"
)

// Environment variable names read by the AMA CLI (the `ama` command in the container).
//
// ⚠️⚠️ **These names must match the constants in cmd/ama-cli/main.go exactly**
// (envToken / envBaseURL / envWorkspaceID). If the names do not match, there is **no error**:
// the CLI cannot read the token and says "the platform did not configure injection", while our
// logs show injection succeeded.
const (
	// EnvAMAToken is the AMA API Key, put into Authorization: Bearer. **Required by the CLI.**
	EnvAMAToken = "AMA_TOKEN"

	// EnvAMABaseURL is the service address. The CLI in the container uses it to know which AMA
	// to call.
	EnvAMABaseURL = "AMA_BASE_URL"

	// EnvAMAWorkspaceID is the workspace the current session belongs to.
	//
	// ⚠️ **Pass the workspace id (ws_xxx), not the key in the URL.** The CLI uses it directly to
	// build `/api/workspaces/{id}/...`; passing the key gives 404.
	EnvAMAWorkspaceID = "AMA_WORKSPACE_ID"

	// EnvAMASessionID is the id of the current session (the Ark session), which is also the Run id.
	EnvAMASessionID = "AMA_SESSION_ID"
)

// The two environment variable names read by lark-cli (the Feishu CLI in the container).
//
// ⚠️ The `LARKSUITE_CLI_` prefix was found by testing: lark-cli does not read near names like
// `LARK_APP_ID`. With a wrong name it returns `not_configured`, which looks like "env is not
// supported".
//
// ⚠️ **Both must appear together**: if one is missing, lark-cli reports "missing app id", and
// that message points to something the user cannot fix.
const (
	// EnvLarkUserAccessToken is the **user** access_token. **It is a secret**; it is in secretEnvNames.
	EnvLarkUserAccessToken = "LARKSUITE_CLI_USER_ACCESS_TOKEN"

	// EnvLarkAppID is which Feishu app that token belongs to. Not a secret (cli_xxx is a public id).
	EnvLarkAppID = "LARKSUITE_CLI_APP_ID"
)

// LarkUserEnvs builds the two variables that let lark-cli work as the user (always given together).
func LarkUserEnvs(token, appID string) map[string]string {
	return map[string]string{
		EnvLarkUserAccessToken: token,
		EnvLarkAppID:           appID,
	}
}

// SessionEnvSource provides "which environment variables to inject for this Ark session".
//
// The scrub policy depends on the key: credentials must be scrubbed from output (see scrub.go),
// while plain env the user filled in should not be scrubbed. Those are his own settings, and
// scrubbing them would make command output confusing to him. So this interface reports
// separately "which keys hold secret values".
type SessionEnvSource interface {
	// SessionEnvs gets the environment variables to inject for this session.
	//
	// secretKeys are the keys in envs **whose values must be scrubbed from output**; a subset of envs.
	//
	// aliases are "credential alias" declarations (see EnvAlias). ⚠️ **They only have names, no
	// values.** The value is taken at injection time from the **merged result**: that way an alias
	// naturally inherits the decision "did the agent enable this authorization". Fetching the value
	// on its own would become a hole that bypasses authorization.
	//
	// When nothing is found it returns nil, nil, nil, nil. **That is not an error**; it means "no
	// extra variables".
	SessionEnvs(ctx context.Context, arkSessionID string) (
		envs map[string]string, secretKeys []string, aliases []EnvAlias, err error)
}

// platformReservedEnvs are variable names that **user-filled env may not override**.
//
// # This is a privilege escalation hole, not tidiness
//
// Without this check, a user could put `AMA_TOKEN=<stolen key>` into the agent's custom env.
// If user env won the merge, the container would run as **the identity he chose** instead of the
// one the platform computed: trading the permission to "fill a form" for the permission to
// "call the platform as anyone". So the rule is **platform injection always wins**, and each
// blocked entry must be logged (silently dropping it would make the user think the setting took
// effect).
//
// ⚠️ **Every new platform-injected variable must be added to this table.** Forgetting it is not
// a compile error; it means that variable can be quietly swapped by the user. A test checks this
// table against "keys actually injected".
var platformReservedEnvs = map[string]bool{
	EnvAMAToken:       true,
	EnvAMABaseURL:     true,
	EnvAMAWorkspaceID: true,
	EnvAMASessionID:   true,
	// The two Feishu ones are reserved too: filling in someone else's token lets you read their
	// chats as that person, the same kind of escalation.
	EnvLarkUserAccessToken: true,
	EnvLarkAppID:           true,
}

// IsPlatformReservedEnv reports whether a variable name is reserved by the platform.
//
// Exported for core: such keys should be blocked **when the agent is saved**, with a clear
// message, instead of being silently ignored at run time. Both sides are needed: blocking on the
// write path is for user experience, blocking at injection is the **security boundary**.
func IsPlatformReservedEnv(key string) bool {
	return platformReservedEnvs[strings.TrimSpace(key)]
}

// AliasableEnvKeys are the platform variable names **allowed to be referenced by an alias**.
//
// There is only one rule: **its value is itself a usable credential**. AMA_BASE_URL /
// AMA_WORKSPACE_ID / LARKSUITE_CLI_APP_ID are public ids; aliasing them is pointless, so they are
// not here.
//
// ⚠️ **This table must stay consistent with secretEnvNames in scrub.go**: present here but missing
// there = the alias value **is not scrubbed**, so the plain text leaves the container, and
// permanently. A test checks these two tables against each other.
//
// It returns a slice instead of a map: it goes into the HTTP response for the frontend to render
// dropdown options, so the order must be stable.
func AliasableEnvKeys() []string {
	return []string{
		EnvAMAToken,
		EnvLarkUserAccessToken,
	}
}

// IsAliasableEnv reports whether a platform variable can be referenced by an alias.
func IsAliasableEnv(key string) bool {
	key = strings.TrimSpace(key)
	for _, k := range AliasableEnvKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// EnvAlias is a declaration: "inject the value of the platform variable From once more under the
// name Key".
//
// ⚠️ **This package deliberately does not import model/apimodel**, so a local copy is declared
// here. It has the same shape as apimodel.AgentEnvAlias and core converts between them.
type EnvAlias struct {
	Key  string
	From string
}
