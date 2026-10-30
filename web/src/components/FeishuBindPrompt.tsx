/**
 * The call to action when "this Feishu app has no Agent handling it yet".
 *
 * # Why an in-place picker instead of jumping to the Agent list
 *
 * "Not bound" is a state that **needs user action**. Before, it was only a line of gray text ("No binding yet. Bind this app under the
 * "Feishu" tab on the Agent detail page"), which wrote what the user should do as a manual.
 *
 * Jumping to the Agent list does not work either: that page has no context of "you came here to bind Feishu", and the bind entry is in **the Feishu tab
 * of the detail page, one more click in**, turning one click into three, and the user can get lost in either of the two middle steps.
 *
 * And **one Feishu app can only be bound to one Agent** (1:1 exclusive, see core.Service.ensureFeishuAppsFree),
 * so the decision the user makes is "pick one of several", which is exactly the shape of a Select, not a
 * navigation. Bind in place after choosing, and see the result right there.
 *
 * # Saving must send back the full draft
 *
 * The frontend signature of `updateAgent` is `Partial<AgentDraft>`, but **the server PATCH takes the full
 * draft** (updateAgent in core/handler.go decodes straight into domain.AgentDraft). Sending only
 * `{feishuAppIds}` makes name an empty string, rejected by validateAgentDraft as 400, so here we use
 * `toAgentDraft(agent)` to get everything and then change that one field. Missing fields fail in different ways (name gives 400,
 * credentialMode silently changes credential injection), but none of them points to "I only wanted to bind Feishu".
 */

import { useState } from "react";
import { Button, ListBox, Select } from "@heroui/react";
import { Link } from "react-router-dom";
import { api } from "@/data/api";
import { useWorkspacePath } from "@/app/WorkspaceGate";
import { usePrefs } from "@/prefs/PrefsContext";
import { toAgentDraft, type Agent, type FeishuApp } from "@/types/domain";

interface FeishuBindPromptProps {
  app: FeishuApp;
  /**
   * All Agents in the workspace.
   *
   * **`null` means not loaded yet** (loading or load failed). We must not render nothing then: that falls back to
   * the original "not bound and no entry". Give a link to the Agent list as fallback: it really works in both cases,
   * and does not lie on load failure the way "no Agent yet" would.
   */
  agents: Agent[] | null;
  /** After binding succeeds, hand the updated agent back to the caller to refresh the reverse references in place. */
  onBound: (agent: Agent) => void;
}

export function FeishuBindPrompt({ app, agents, onBound }: FeishuBindPromptProps) {
  const { t } = usePrefs();
  const wp = useWorkspacePath();
  const [picked, setPicked] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  async function bind() {
    if (!picked || !agents) return;
    const agent = agents.find((a) => a.id === picked);
    if (!agent) return;
    setSaving(true);
    setError("");
    try {
      // Send everything back, change only feishuAppIds; see the file header for why.
      const draft = toAgentDraft(agent);
      const next = await api.updateAgent(agent.id, {
        ...draft,
        feishuAppIds: [...draft.feishuAppIds, app.id],
      });
      onBound(next);
    } catch (e) {
      // Taking over makes the server return 409, and that message includes the current holder's name; showing it as is is more accurate than any fallback text.
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex flex-col gap-2" data-testid={`bind-agent-prompt-${app.appId}`}>
      <p className="text-sm text-muted">{t("feishu.bindPromptHint")}</p>

      {agents === null ? (
        // Fallback exit when the agent list cannot be loaded; see the note on props.
        <div>
          <Link to={wp("/agents")} className="no-underline">
            <Button data-testid={`bind-agent-fallback-${app.appId}`}>
              {t("feishu.bindPromptAction")}
            </Button>
          </Link>
        </div>
      ) : agents.length === 0 ? (
        // No Agent at all in the workspace: a picker is pointless, so offer "go create" directly.
        <div className="flex flex-col gap-2">
          <p className="text-sm text-muted">{t("feishu.bindNoAgents")}</p>
          <div>
            <Link to={wp("/agents/new")} className="no-underline">
              <Button data-testid={`bind-agent-create-${app.appId}`}>{t("agent.new")}</Button>
            </Link>
          </div>
        </div>
      ) : (
        <div className="flex flex-wrap items-end gap-2">
          <Select
            className="w-56"
            // The card itself is a surface; the picker uses secondary so it does not stack another highlight.
            variant="secondary"
            aria-label={t("feishu.bindPickAgent")}
            placeholder={t("feishu.bindPickAgent")}
            selectedKey={picked}
            onSelectionChange={(k) => setPicked(k === null ? null : String(k))}
          >
            <Select.Trigger data-testid={`bind-agent-select-${app.appId}`}>
              <Select.Value />
              <Select.Indicator />
            </Select.Trigger>
            <Select.Popover>
              <ListBox>
                {agents.map((a) => (
                  <ListBox.Item key={a.id} id={a.id} textValue={a.name}>
                    {a.name}
                  </ListBox.Item>
                ))}
              </ListBox>
            </Select.Popover>
          </Select>
          <Button
            isDisabled={picked === null}
            isPending={saving}
            onPress={() => void bind()}
            data-testid={`bind-agent-submit-${app.appId}`}
          >
            {t("feishu.bindPromptAction")}
          </Button>
        </div>
      )}

      {error ? (
        <p className="text-sm text-danger" data-testid={`bind-agent-error-${app.appId}`}>
          {error}
        </p>
      ) : null}
    </div>
  );
}
