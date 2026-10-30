// This file holds small helpers shared by all resources. The SDK wraps optional values in
// ogen-generated OptXxx types, which are verbose to build by hand (agent.NewOptString(...)),
// so they are built in one place here.
//
// Note: OptString and friends are generated separately in every model sub-package and are not
// interchangeable, so the helpers below are provided per package; one set is not enough.

package ma

import (
	"github.com/volcengine/ark-runtime-go/arkruntime/model/agent"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/environment"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/memory"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/session"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/vault"
)

// Built-in toolset. Pass it as an element of AgentSpec.Tools to enable the platform's built-in
// tools (bash / text_editor / web_fetch, etc.).
//
// The toolset is versioned by a date suffix, and Ark may release new versions later. To pin a
// specific version, pass the string literal directly; there is no need to wait for this package
// to update the constant.
const ToolsetDefault = "agent_toolset_20260401"

// Environment types: cloud is the Ark-hosted sandbox, self_hosted is a self-hosted Worker.
const (
	EnvCloud      = string(environment.EnvConfigTypeCloud)
	EnvSelfHosted = string(environment.EnvConfigTypeSelfHosted)
)

// Network policy: unrestricted allows all outbound traffic, limited restricts it to an allowlist.
const (
	NetworkUnrestricted = string(environment.NetworkingTypeUnrestricted)
	NetworkLimited      = string(environment.NetworkingTypeLimited)
)

// optAgentString wraps a non-empty string in agent.OptString; an empty string stays "unset".
//
// Why empty means unset: these fields are all optional on the wire, and sending an explicit
// empty string means something different from not sending it (some fields reject an empty
// string as invalid). Callers that want to clear a field explicitly should use the *Raw methods.
func optAgentString(s string) agent.OptString {
	if s == "" {
		return agent.OptString{}
	}
	return agent.NewOptString(s)
}

func optEnvString(s string) environment.OptString {
	if s == "" {
		return environment.OptString{}
	}
	return environment.NewOptString(s)
}

func optSessionString(s string) session.OptString {
	if s == "" {
		return session.OptString{}
	}
	return session.NewOptString(s)
}

func optMemoryString(s string) memory.OptString {
	if s == "" {
		return memory.OptString{}
	}
	return memory.NewOptString(s)
}

func optVaultString(s string) vault.OptString {
	if s == "" {
		return vault.OptString{}
	}
	return vault.NewOptString(s)
}

// optAgentInt32 / optSessionInt32 etc.: used for the page limit. 0 means not sent (server default).
func optAgentInt32(v int32) agent.OptInt32 {
	if v == 0 {
		return agent.OptInt32{}
	}
	return agent.NewOptInt32(v)
}

func optEnvInt32(v int32) environment.OptInt32 {
	if v == 0 {
		return environment.OptInt32{}
	}
	return environment.NewOptInt32(v)
}

func optSessionInt32(v int32) session.OptInt32 {
	if v == 0 {
		return session.OptInt32{}
	}
	return session.NewOptInt32(v)
}

func optVaultInt32(v int32) vault.OptInt32 {
	if v == 0 {
		return vault.OptInt32{}
	}
	return vault.NewOptInt32(v)
}

func optMemoryInt32(v int32) memory.OptInt32 {
	if v == 0 {
		return memory.OptInt32{}
	}
	return memory.NewOptInt32(v)
}

// Page is the pagination parameter shared by all List APIs.
//
// Ark uses "numeric page + next_page cursor" instead of offset: leave Page empty for the first
// page, then copy NextPage from the previous response back in as is.
type Page struct {
	// Limit is the max number of items per page; 0 uses the server default.
	Limit int32
	// Page is the page cursor; leave empty for the first page, then use next_page from the
	// previous response.
	Page string
	// CreatedAtGte / CreatedAtLte filter by creation time, in RFC 3339 format.
	CreatedAtGte string
	CreatedAtLte string
}
