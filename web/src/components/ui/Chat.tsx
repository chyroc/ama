/**
 * Building blocks for the chat UI: scroll shell, messages, collapsible thinking process, loading signals.
 *
 * - `ChatConversation`: a scrollable message column. When parked at the bottom, new content follows to the bottom automatically;
 *   when the user scrolls up to read history it no longer yanks them back, and shows a "back to bottom" button instead.
 * - `ChatMessage`: User (bubble on the right) / Assistant (avatar + body, no bubble).
 * - `ChainOfThought`: a collapsible process panel built on the OSS Disclosure;
 *   both controlled (isExpanded/onExpandedChange) and uncontrolled (defaultExpanded) are supported.
 * - `TextShimmer` / `ChatLoader.Dots`: two kinds of "it is working" signals.
 *
 * BEM class names (`chat-message__content` etc.) are only hooks for tests and local overrides;
 * the styles themselves are all Tailwind classes here plus a few keyframes in index.css.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type HTMLAttributes,
  type ReactNode,
  type RefObject,
} from "react";
import { Avatar, Button, Disclosure } from "@heroui/react";
import { Icon } from "@iconify/react";

function cx(...parts: (string | undefined | false)[]) {
  return parts.filter(Boolean).join(" ");
}

/* ------------------------------------------------------------------ */
/* ChatConversation                                                    */
/* ------------------------------------------------------------------ */

interface ConversationCtx {
  scrollRef: RefObject<HTMLDivElement | null>;
  atBottom: boolean;
  scrollToBottom: (behavior?: ScrollBehavior) => void;
}

const ConversationContext = createContext<ConversationCtx | null>(null);

/** How many pixels from the bottom still count as "at the bottom". A little margin, so fractional pixels do not make the check flip back and forth. */
const BOTTOM_SLOP = 48;

function ChatConversationRoot({
  className,
  resize = "smooth",
  children,
  ...props
}: HTMLAttributes<HTMLDivElement> & { resize?: "smooth" | "instant" }) {
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const [atBottom, setAtBottom] = useState(true);
  // Keep a copy in a ref for the ResizeObserver callback, so it does not read a stale closure value.
  const atBottomRef = useRef(true);

  const scrollToBottom = useCallback((behavior: ScrollBehavior = "smooth") => {
    const el = scrollRef.current;
    if (el) el.scrollTo({ top: el.scrollHeight, behavior });
  }, []);

  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const onScroll = () => {
      const next = el.scrollHeight - el.scrollTop - el.clientHeight <= BOTTOM_SLOP;
      atBottomRef.current = next;
      setAtBottom(next);
    };
    el.addEventListener("scroll", onScroll, { passive: true });

    // Content height changed (streaming output, expand/collapse) and we were at the bottom → follow to the bottom.
    const ro = new ResizeObserver(() => {
      if (atBottomRef.current) scrollToBottom(resize === "smooth" ? "smooth" : "auto");
    });
    for (const child of Array.from(el.children)) ro.observe(child);
    // On first mount, jump straight to the bottom without animation.
    scrollToBottom("auto");
    return () => {
      el.removeEventListener("scroll", onScroll);
      ro.disconnect();
    };
  }, [resize, scrollToBottom]);

  return (
    <ConversationContext.Provider value={{ scrollRef, atBottom, scrollToBottom }}>
      <div {...props} className={cx("chat-conversation relative flex min-h-0 flex-col", className)}>
        {/* data-slot is a convention name: ChatComposer uses it to find
            the scrolling layer (STREAM_SELECTOR), and e2e locates by it too. */}
        <div
          ref={scrollRef}
          data-slot="chat-conversation"
          className="flex min-h-0 flex-1 flex-col overflow-y-auto"
        >
          {children}
        </div>
      </div>
    </ConversationContext.Provider>
  );
}

function ChatConversationContent({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  // No w-full: this uses mx-auto + max-w to center, so the caller's max-w-* takes effect.
  return <div {...props} className={cx("chat-conversation__content mx-auto flex w-full", className)} />;
}

/** Bottom placeholder: gives the scroll container a clear "last row"; no other behavior. */
function ChatConversationScrollAnchor() {
  return <div aria-hidden className="h-px shrink-0" />;
}

function ChatConversationScrollButton({ "aria-label": ariaLabel }: { "aria-label"?: string }) {
  const ctx = useContext(ConversationContext);
  if (!ctx || ctx.atBottom) return null;
  return (
    // Floats above the scroll area: placed in the outer container (not inside the scroll area), so it does not scroll away with content.
    <div className="pointer-events-none absolute inset-x-0 bottom-3 flex justify-center">
      <Button
        isIconOnly
        size="sm"
        variant="secondary"
        aria-label={ariaLabel}
        className="pointer-events-auto rounded-full shadow-md"
        onPress={() => ctx.scrollToBottom("smooth")}
      >
        <Icon icon="gravity-ui:arrow-down" className="size-4" />
      </Button>
    </div>
  );
}

export const ChatConversation = Object.assign(ChatConversationRoot, {
  Content: ChatConversationContent,
  ScrollAnchor: ChatConversationScrollAnchor,
  ScrollButton: ChatConversationScrollButton,
});

/* ------------------------------------------------------------------ */
/* ChatMessage                                                         */
/* ------------------------------------------------------------------ */

type DivProps = HTMLAttributes<HTMLDivElement>;

function ChatMessageUser({ className, ...props }: DivProps) {
  return <div {...props} className={cx("chat-message chat-message--user flex justify-end", className)} />;
}

function ChatMessageAssistant({ className, ...props }: DivProps) {
  return (
    <div {...props} className={cx("chat-message chat-message--assistant flex items-start gap-3", className)} />
  );
}

function ChatMessageBubble({ className, ...props }: DivProps) {
  return (
    <div
      {...props}
      className={cx(
        "chat-message__bubble max-w-[85%] min-w-0 rounded-2xl bg-default px-4 py-2.5 text-foreground",
        className,
      )}
    />
  );
}

function ChatMessageBody({ className, ...props }: DivProps) {
  return <div {...props} className={cx("chat-message__body flex min-w-0 flex-1 flex-col gap-2 pt-0.5", className)} />;
}

function ChatMessageContent({ className, ...props }: DivProps) {
  return (
    <div
      data-slot="chat-message-content"
      {...props}
      className={cx("chat-message__content text-sm leading-relaxed text-foreground", className)}
    />
  );
}

function ChatMessageAvatar({ alt, fallback, src }: { alt: string; fallback: ReactNode; src?: string }) {
  return (
    <Avatar size="sm" className="shrink-0">
      {src ? <Avatar.Image src={src} alt={alt} /> : null}
      <Avatar.Fallback aria-label={alt}>{fallback}</Avatar.Fallback>
    </Avatar>
  );
}

export const ChatMessage = {
  User: ChatMessageUser,
  Assistant: ChatMessageAssistant,
  Bubble: ChatMessageBubble,
  Body: ChatMessageBody,
  Content: ChatMessageContent,
  Avatar: ChatMessageAvatar,
};

/* ------------------------------------------------------------------ */
/* TextShimmer / ChatLoader                                            */
/* ------------------------------------------------------------------ */

/** A light sweep across the text from left to right. The animation is `.ama-shimmer` in index.css; it stops under reduced motion. */
export function TextShimmer({ className, children }: { className?: string; children: ReactNode }) {
  return <span className={cx("ama-shimmer", className)}>{children}</span>;
}

function ChatLoaderDots({
  size = "md",
  label,
  className,
}: {
  size?: "sm" | "md";
  label?: string;
  className?: string;
}) {
  const dot = size === "sm" ? "size-1" : "size-1.5";
  return (
    <span role="status" aria-label={label} className={cx("inline-flex items-center gap-1", className)}>
      {[0, 1, 2].map((i) => (
        <span
          key={i}
          aria-hidden
          className={cx("ama-dot rounded-full bg-muted", dot)}
          style={{ animationDelay: `${i * 160}ms` }}
        />
      ))}
    </span>
  );
}

export const ChatLoader = { Dots: ChatLoaderDots };

/* ------------------------------------------------------------------ */
/* ChainOfThought                                                      */
/* ------------------------------------------------------------------ */

const CoTContext = createContext<{ isStreaming: boolean }>({ isStreaming: false });

function ChainOfThoughtRoot({
  isExpanded,
  defaultExpanded,
  onExpandedChange,
  isStreaming = false,
  className,
  children,
  ...rest
}: {
  isExpanded?: boolean;
  defaultExpanded?: boolean;
  onExpandedChange?: (expanded: boolean) => void;
  isStreaming?: boolean;
  className?: string;
  children: ReactNode;
  "data-testid"?: string;
}) {
  return (
    <CoTContext.Provider value={{ isStreaming }}>
      <Disclosure
        {...rest}
        isExpanded={isExpanded}
        defaultExpanded={defaultExpanded}
        onExpandedChange={onExpandedChange}
        className={cx("chain-of-thought", className)}
      >
        {children}
      </Disclosure>
    </CoTContext.Provider>
  );
}

function ChainOfThoughtTrigger({ children, ...rest }: { children: ReactNode; "data-testid"?: string }) {
  const { isStreaming } = useContext(CoTContext);
  return (
    <Disclosure.Heading>
      <Disclosure.Trigger
        {...rest}
        className="flex items-center gap-1.5 text-sm text-muted hover:text-foreground cursor-[var(--cursor-interactive)]"
      >
        {isStreaming ? <TextShimmer>{children}</TextShimmer> : children}
        <Disclosure.Indicator>
          <Icon icon="gravity-ui:chevron-down" className="size-3.5" />
        </Disclosure.Indicator>
      </Disclosure.Trigger>
    </Disclosure.Heading>
  );
}

function ChainOfThoughtContent({ children }: { children: ReactNode }) {
  return (
    <Disclosure.Content>
      <Disclosure.Body className="pt-2">{children}</Disclosure.Body>
    </Disclosure.Content>
  );
}

function ChainOfThoughtSteps({ className, ...props }: HTMLAttributes<HTMLOListElement>) {
  // A thin vertical line on the left links the steps, visually part of the same block as the trigger above.
  return <ol {...props} className={cx("ml-1.5 flex flex-col gap-2 border-l border-border pl-4", className)} />;
}

function ChainOfThoughtStep({ className, ...props }: HTMLAttributes<HTMLLIElement>) {
  return <li {...props} className={cx("min-w-0", className)} />;
}

export const ChainOfThought = Object.assign(ChainOfThoughtRoot, {
  Trigger: ChainOfThoughtTrigger,
  Content: ChainOfThoughtContent,
  Steps: ChainOfThoughtSteps,
  Step: ChainOfThoughtStep,
});
