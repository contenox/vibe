import { cn } from "../utils";

type ProgressBarProps = {
  value: number;
  palette?: "neutral" | "success" | "warning" | "primary" | "error";
  className?: string;
};

export function ProgressBar({
  value,
  palette = "neutral",
  className,
}: ProgressBarProps) {
  return (
    <div
      className={cn(
        "h-2 bg-surface-200 rounded-full overflow-hidden",
        className,
      )}
    >
      <div
        className={cn("h-full transition-all duration-500 ease-out", {
          "bg-surface-300": palette === "neutral",
          "bg-green-500 dark:bg-dark-success-500": palette === "success",
          "bg-yellow-500 dark:bg-dark-warning-500": palette === "warning",
          "bg-primary-500": palette === "primary",
          "bg-red-500 dark:bg-dark-error-500": palette === "error",
        })}
        style={{ width: `${value}%` }}
      />
    </div>
  );
}
