/**
 * Chat message stream: the ChatConversation scroll shell + the streaming draft.
 *
 * **The per-turn collapsed view is in ConversationView.tsx**; this file only does three things specific to the chat page:
 * the scroll container (with the bottom anchor and "back to bottom"), the streaming draft, and the thinking signal before any text appears.
 * The run detail page uses ConversationView directly (normal page-flow scrolling, without this shell);
 * see the top of that file for why.
 */

import { ChainOfThought, ChatConversation, ChatMessage, TextShimmer } from "@/components/ui/Chat";
import { ConversationView } from "@/components/ConversationView";
import { MarkdownMessage } from "@/components/MarkdownMessage";
import { buildTimeline } from "@/lib/timeline";
import { useT } from "@/prefs/PrefsContext";
import type { MessageDraft } from "@/lib/useConversationStream";
import type { RunEvent } from "@/types/events";

interface ChatStreamProps {
  events: RunEvent[];
  draft: MessageDraft | null;
  /** Streaming thinking process. Kept separate from draft; see the comment on useConversationStream for why. */
  thinkingDraft: MessageDraft | null;
  thinking: boolean;
  /** Agent name, used as the avatar fallback and accessibility label. */
  agentName: string;
}

export function ChatStream({
  events,
  draft,
  thinkingDraft,
  thinking,
  agentName,
}: ChatStreamProps) {
  const t = useT();
  const items = buildTimeline(events, t);

  return (
    <ChatConversation className="flex-1" resize="smooth">
      {/*
        ChatConversation.Content already has `mx-auto w-full`; here we only add max-w-3xl,
        so the message column is centered.
      */}
      <ChatConversation.Content className="max-w-3xl flex-col gap-5 px-4 pt-8 pb-6">
        <ConversationView items={items} agentName={agentName} />

        {/*
          Streaming thinking process.

          **Collapsed by default** (`defaultExpanded={false}`), same as the settled form, so when thinking
          ends and the settled events take over rendering, the expanded state does not jump. isStreaming gives the trigger a
          shimmer, i.e. the live "thinking" state.
        */}
        {thinkingDraft?.text ? (
          <ChainOfThought defaultExpanded={false} isStreaming data-testid="chat-thinking-draft">
            <ChainOfThought.Trigger>{t("chat.thinking")}</ChainOfThought.Trigger>
            <ChainOfThought.Content>
              <ChainOfThought.Steps>
                <ChainOfThought.Step>
                  <MarkdownMessage streaming className="text-xs leading-relaxed text-muted">
                    {thinkingDraft.text}
                  </MarkdownMessage>
                </ChainOfThought.Step>
              </ChainOfThought.Steps>
            </ChainOfThought.Content>
          </ChainOfThought>
        ) : null}

        {/* Streaming draft: looks the same as the settled agent message, to avoid a jump when it settles. */}
        {draft ? (
          <ChatMessage.Assistant data-testid="chat-draft">
            <ChatMessage.Avatar alt={agentName} fallback={agentName.slice(0, 1) || "A"} />
            <ChatMessage.Body>
              {/*
                The draft also uses MarkdownMessage, and must use **the same** component as the settled form: if the two renderers differ,
                at the moment the last frame settles the whole block jumps from plain text to formatted Markdown, which looks like a flash.
              */}
              <ChatMessage.Content>
                <MarkdownMessage streaming>{draft.text}</MarkdownMessage>
              </ChatMessage.Content>
            </ChatMessage.Body>
          </ChatMessage.Assistant>
        ) : null}

        {/*
          Thinking and nothing output yet: only now do we need an "it is working" signal.
          Once thinking text is streaming, the collapsible panel above takes over, so this line is not shown again.
        */}
        {thinking && !draft && !thinkingDraft?.text ? (
          <div className="flex items-center gap-2 px-2" data-testid="chat-thinking">
            <TextShimmer className="text-sm">{t("chat.thinking")}</TextShimmer>
          </div>
        ) : null}

        <ChatConversation.ScrollAnchor />
      </ChatConversation.Content>

      {/* When scrolled up to read history, show "back to bottom" instead of yanking the viewport back. */}
      <ChatConversation.ScrollButton aria-label={t("chat.title")} />
    </ChatConversation>
  );
}
