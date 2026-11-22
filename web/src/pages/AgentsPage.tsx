import { ThemedComponent } from "@/components/ui/ThemedComponent";
import { ClassicAgentsPage } from "@/components/agents/ClassicAgentsPage";
import { AzureAgentsPage } from "@/components/agents/AzureAgentsPage";

export { CodexAgentNotice, emptyAgentDraft, ModelPicker } from "@/components/agents/ClassicAgentsPage";
export { NewAgentPage } from "@/components/agents/ClassicAgentsPage";

/** Agents page semantic contract; implementations own their whole visual structure. */
export function AgentsPage() {
  return <ThemedComponent classic={() => <ClassicAgentsPage />} azure={() => <AzureAgentsPage />} />;
}
