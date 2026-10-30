// Store contract test for Ark resource bindings, hooked into TestStoreSuite to run once each against MemStore and
// PGStore.

package coredao

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// testRepointArkEnvironment pins the targeted repoint used for "environment 404 self-heal":
// rows in the same workspace still pointing at the old environment (column and doc together) move to the new one; other rows, other fields
// and other workspaces are left alone; when the old environment is no longer referenced, 0 rows change (CAS guard).
//
// It must not degrade into "read the whole row and Put it back": sibling agents catching up skills/prompts also write this table,
// and read-modify-write would overwrite the fingerprint they just wrote. So the PG implementation uses jsonb_set to touch only one key.
func testRepointArkEnvironment(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	mk := func(workspaceID, agentID, envID string) persistmodel.ArkBinding {
		return persistmodel.ArkBinding{
			WorkspaceID:       workspaceID,
			AgentID:           agentID,
			ArkAgentID:        "ark-" + agentID,
			ArkEnvironmentID:  envID,
			SkillsFingerprint: "keep-me-" + agentID,
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutArkBinding(ctx, mk("ws_tmp", "a1", "env-dead")))
	must(s.PutArkBinding(ctx, mk("ws_tmp", "a2", "env-dead")))
	must(s.PutArkBinding(ctx, mk("ws_tmp", "a3", "env-other")))  // other environment, untouched
	must(s.PutArkBinding(ctx, mk("ws_other", "a4", "env-dead"))) // other workspace, untouched

	n, err := s.RepointArkEnvironment(ctx, "ws_tmp", "env-dead", "env-new")
	must(err)
	if n != 2 {
		t.Fatalf("应改 2 行，实际 %d", n)
	}

	for _, want := range []struct {
		workspace, agent, env string
	}{
		{"ws_tmp", "a1", "env-new"},
		{"ws_tmp", "a2", "env-new"},
		{"ws_tmp", "a3", "env-other"},
		{"ws_other", "a4", "env-dead"},
	} {
		b, err := s.GetArkBinding(ctx, want.workspace, want.agent)
		must(err)
		if b.ArkEnvironmentID != want.env {
			t.Errorf("%s/%s 环境应是 %s，实际 %s",
				want.workspace, want.agent, want.env, b.ArkEnvironmentID)
		}
		// Repointing the row must not wipe other fields in doc, nor change the extracted ark_agent_id column.
		if b.SkillsFingerprint != "keep-me-"+want.agent {
			t.Errorf("%s 的其它 doc 字段被抹了：fp=%q", want.agent, b.SkillsFingerprint)
		}
		if b.ArkAgentID != "ark-"+want.agent {
			t.Errorf("%s 的 ark_agent_id 被改了：%s", want.agent, b.ArkAgentID)
		}
	}

	// CAS guard: the old environment is no longer referenced, so another repoint changes 0 rows.
	n2, err := s.RepointArkEnvironment(ctx, "ws_tmp", "env-dead", "env-new2")
	must(err)
	if n2 != 0 {
		t.Errorf("已无旧环境，应改 0 行，实际 %d", n2)
	}
}
