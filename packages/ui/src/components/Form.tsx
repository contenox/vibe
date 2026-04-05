import { cn } from "../utils";
import { Panel } from "./Panel";
import { H2 } from "./Typography";

interface FormProps
  extends Omit<React.HTMLAttributes<HTMLFormElement>, "onError"> {
  title?: string;
  onSubmit: (e: React.FormEvent) => void;
  error?: string;
  onError?: (error: string) => void;
  actions?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  variant?:
    | "default"
    | "raised"
    | "flat"
    | "bordered"
    | "error"
    | "gradient"
    | "surface"
    | "ghost"
    | "empty"
    | "body";
}

export function Form({
  title,
  onSubmit,
  error,
  onError,
  actions,
  variant = "default",
  children,
  className,
}: FormProps) {
  return (
    <Panel variant={variant} className={className}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          try {
            onSubmit(e);
          } catch (err) {
            onError?.(err instanceof Error ? err.message : String(err));
          }
        }}
        className="space-y-6"
      >
        {title && (
          <H2 className="text-text dark:text-dark-text text-2xl font-semibold">
            {title}
          </H2>
        )}

        <div className="space-y-4">{children}</div>

        {error && (
          <Panel variant="error" className="text-sm font-medium">
            {error}
          </Panel>
        )}

        {actions && <div className="flex gap-4">{actions}</div>}
      </form>
    </Panel>
  );
}
