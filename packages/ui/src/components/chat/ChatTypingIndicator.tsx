import { cn } from "../../utils";

export type ChatTypingIndicatorProps = {
  className?: string;
  /** Accessible label (e.g. “Assistant is typing”) */
  "aria-label"?: string;
};

export function ChatTypingIndicator({
  className,
  "aria-label": ariaLabel = "Typing",
}: ChatTypingIndicatorProps) {
  return (
    <div
      className={cn("flex items-center gap-1.5 px-2 py-1", className)}
      role="status"
      aria-label={ariaLabel}
    >
      {[0, 1, 2].map((i) => (
        <span
          key={i}
          className="bg-secondary-400 dark:bg-dark-secondary-500 h-2 w-2 animate-pulse rounded-full"
          style={{ animationDelay: `${i * 180}ms` }}
        />
      ))}
    </div>
  );
}
