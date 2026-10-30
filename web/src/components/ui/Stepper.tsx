/**
 * Horizontal stepper: dots + connecting lines.
 *
 * Usage and compound component shape:
 *
 *   <Stepper currentStep={i} onStepChange={setI} size="sm">
 *     <Stepper.Step>
 *       <Stepper.Indicator />
 *       <Stepper.Separator />
 *       <span className="sr-only">name of the step</span>
 *     </Stepper.Step>
 *   </Stepper>
 *
 * Children of a Step other than Separator (Indicator and the sr-only text) are wrapped in one
 * button, so the sr-only text is the button's accessible name. Without onStepChange the buttons are disabled.
 */

import {
  Children,
  createContext,
  isValidElement,
  useContext,
  type ReactElement,
  type ReactNode,
} from "react";
import { Icon } from "@iconify/react";

type StepState = "completed" | "current" | "upcoming";

interface StepperCtx {
  currentStep: number;
  onStepChange?: (index: number) => void;
  size: "sm" | "md";
}

const RootContext = createContext<StepperCtx>({ currentStep: 0, size: "md" });
const StepContext = createContext<{ index: number; state: StepState; isLast: boolean }>({
  index: 0,
  state: "upcoming",
  isLast: false,
});

function stateOf(index: number, current: number): StepState {
  if (index < current) return "completed";
  if (index === current) return "current";
  return "upcoming";
}

function StepperRoot({
  currentStep,
  onStepChange,
  size = "md",
  className,
  children,
}: {
  currentStep: number;
  onStepChange?: (index: number) => void;
  size?: "sm" | "md";
  className?: string;
  children: ReactNode;
}) {
  const steps = Children.toArray(children).filter(isValidElement);
  return (
    <RootContext.Provider value={{ currentStep, onStepChange, size }}>
      <ol className={`flex w-full items-center ${className ?? ""}`}>
        {steps.map((child, index) => (
          <StepContext.Provider
            key={child.key ?? index}
            value={{
              index,
              state: stateOf(index, currentStep),
              isLast: index === steps.length - 1,
            }}
          >
            {child}
          </StepContext.Provider>
        ))}
      </ol>
    </RootContext.Provider>
  );
}

function StepperStep({ children }: { children: ReactNode }) {
  const { onStepChange } = useContext(RootContext);
  const { index, state, isLast } = useContext(StepContext);

  const parts = Children.toArray(children);
  const separator = parts.find(
    (c) => isValidElement(c) && (c as ReactElement).type === StepperSeparator,
  );
  const rest = parts.filter((c) => c !== separator);

  return (
    <li className={`flex items-center ${isLast ? "" : "flex-1"}`}>
      <button
        type="button"
        disabled={!onStepChange}
        aria-current={state === "current" ? "step" : undefined}
        onClick={() => onStepChange?.(index)}
        className="shrink-0 rounded-full outline-none focus-visible:ring-2 focus-visible:ring-focus disabled:cursor-default enabled:cursor-[var(--cursor-interactive)]"
      >
        {rest}
      </button>
      {isLast ? null : separator}
    </li>
  );
}

const INDICATOR_STATE: Record<StepState, string> = {
  completed: "bg-accent text-accent-foreground border-accent",
  current: "bg-surface text-accent border-accent",
  upcoming: "bg-surface text-muted border-border",
};

function StepperIndicator() {
  const { size } = useContext(RootContext);
  const { index, state } = useContext(StepContext);
  const box = size === "sm" ? "size-6 text-[0.6875rem]" : "size-8 text-xs";
  return (
    <span
      aria-hidden
      className={`flex items-center justify-center rounded-full border-2 font-medium transition-colors ${box} ${INDICATOR_STATE[state]}`}
    >
      {state === "completed" ? <Icon icon="gravity-ui:check" className="size-3.5" /> : index + 1}
    </span>
  );
}

function StepperSeparator() {
  const { state } = useContext(StepContext);
  return (
    <span
      aria-hidden
      className={`mx-1.5 h-0.5 flex-1 rounded-full transition-colors ${
        state === "completed" ? "bg-accent" : "bg-border"
      }`}
    />
  );
}

export const Stepper = Object.assign(StepperRoot, {
  Step: StepperStep,
  Indicator: StepperIndicator,
  Separator: StepperSeparator,
});
