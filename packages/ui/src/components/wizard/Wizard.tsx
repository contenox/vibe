import { cn } from "../../utils";
import { H3, P } from "../Typography";
import { Panel } from "../Panel";

export type WizardProps = {
  title?: React.ReactNode;
  description?: React.ReactNode;
  footer?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
};

/**
 * Vertical setup / onboarding container. Presentational only — pass step state from the app.
 */
export function Wizard({
  title,
  description,
  footer,
  children,
  className,
}: WizardProps) {
  return (
    <Panel
      variant="bordered"
      className={cn(
        "border-amber-500/60 bg-amber-500/5 text-inherit shrink-0 px-4 py-4",
        className,
      )}
    >
      {(title || description) && (
        <header className="mb-4 space-y-1">
          {title ? (
            <H3 variant="subsectionTitle" className="text-balance">
              {title}
            </H3>
          ) : null}
          {description ? (
            <P variant="muted" className="text-sm">
              {description}
            </P>
          ) : null}
        </header>
      )}
      <div className="space-y-0">{children}</div>
      {footer ? <footer className="mt-4 border-t border-surface-200 pt-4 dark:border-dark-surface-700">{footer}</footer> : null}
    </Panel>
  );
}
