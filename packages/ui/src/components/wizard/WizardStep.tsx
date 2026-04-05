import { AlertCircle, Check } from "lucide-react";
import { cn } from "../../utils";
import { H3, P } from "../Typography";

export type WizardStepStatus = "complete" | "current" | "upcoming" | "error";

export type WizardStepProps = {
  step: number;
  status: WizardStepStatus;
  /** First incomplete / actionable step: sets aria-current="step" (use even when status is error). */
  active?: boolean;
  title: React.ReactNode;
  description?: React.ReactNode;
  children?: React.ReactNode;
  /** When false, the rail shows a connector line below the indicator (default true). */
  isLast?: boolean;
  className?: string;
};

function StepIndicator({
  step,
  status,
}: {
  step: number;
  status: WizardStepStatus;
}) {
  const base =
    "flex h-9 w-9 shrink-0 items-center justify-center rounded-full border-2 text-sm font-semibold transition-colors";

  if (status === "complete") {
    return (
      <span
        className={cn(
          base,
          "border-emerald-600 bg-emerald-600 text-white dark:border-emerald-500 dark:bg-emerald-600",
        )}
        aria-hidden
      >
        <Check className="h-4 w-4" strokeWidth={2.5} />
      </span>
    );
  }

  if (status === "error") {
    return (
      <span
        className={cn(
          base,
          "border-destructive bg-destructive/10 text-destructive dark:border-red-500 dark:text-red-400",
        )}
        aria-hidden
      >
        <AlertCircle className="h-4 w-4" />
      </span>
    );
  }

  if (status === "current") {
    return (
      <span
        className={cn(
          base,
          "border-primary-600 bg-primary-50 text-primary-700 ring-2 ring-primary-200 ring-offset-2 ring-offset-amber-500/5 dark:border-primary-400 dark:bg-primary-950 dark:text-primary-200 dark:ring-primary-800",
        )}
        aria-hidden
      >
        {step}
      </span>
    );
  }

  return (
    <span
      className={cn(
        base,
        "border-surface-300 bg-surface-50 text-muted-foreground dark:border-dark-surface-600 dark:bg-dark-surface-800",
      )}
      aria-hidden
    >
      {step}
    </span>
  );
}

/**
 * One row in a vertical wizard: rail (number / icon) + title, description, and slot content.
 */
export function WizardStep({
  step,
  status,
  active = false,
  title,
  description,
  children,
  isLast = false,
  className,
}: WizardStepProps) {
  const lineClass =
    status === "complete"
      ? "bg-emerald-500/50 dark:bg-emerald-600/40"
      : "bg-surface-200 dark:bg-dark-surface-600";

  return (
    <div
      className={cn("flex gap-4", className)}
      aria-current={active || status === "current" ? "step" : undefined}
    >
      <div className="flex w-9 shrink-0 flex-col items-center">
        <StepIndicator step={step} status={status} />
        {!isLast ? (
          <div
            className={cn("mt-2 w-px flex-1 min-h-[1.5rem]", lineClass)}
            aria-hidden
          />
        ) : null}
      </div>
      <div className={cn("min-w-0 flex-1", !isLast && "pb-6")}>
        <H3
          variant="subsectionTitle"
          className={cn(
            "text-base",
            status === "complete" && "text-muted-foreground",
            status === "upcoming" && "text-muted-foreground",
          )}
        >
          {title}
        </H3>
        {description ? (
          <P variant="muted" className="mt-1 text-sm">
            {description}
          </P>
        ) : null}
        {children ? <div className="mt-3 space-y-2">{children}</div> : null}
      </div>
    </div>
  );
}
