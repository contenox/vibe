import { CheckCircle2, XCircle } from "lucide-react";
import { cn } from "../utils";
import { Span } from "./Typography";

type ToastProps = {
  message: string;
  variant: "success" | "error";
  className?: string;
};

export function Toast({ message, variant, className }: ToastProps) {
  return (
    <div
      className={cn(
        "fixed bottom-4 left-1/2 -translate-x-1/2 rounded-lg p-4 shadow-lg",
        "flex items-center gap-3",
        variant === "success"
          ? "bg-primary-500 text-surface-50 dark:bg-dark-primary-600"
          : "bg-error-500 text-surface-50 dark:bg-dark-error-600",
        className,
      )}
    >
      {variant === "success" ? (
        <CheckCircle2 className="h-5 w-5" />
      ) : (
        <XCircle className="h-5 w-5" />
      )}
      <Span className="text-sm font-medium">{message}</Span>
    </div>
  );
}
