import type { RefObject } from "react";
import { cn } from "../../utils";

export type ChatThreadProps = {
  containerRef: RefObject<HTMLDivElement | null>;
  endRef: RefObject<HTMLDivElement | null>;
  children: React.ReactNode;
  className?: string;
  scrollClassName?: string;
  /** Scroll region `aria-live` (default polite). Set false to omit. */
  ariaLive?: false | "off" | "polite" | "assertive";
};

export function ChatThread({
  containerRef,
  endRef,
  children,
  className,
  scrollClassName,
  ariaLive = "polite",
}: ChatThreadProps) {
  const liveProps =
    ariaLive === false
      ? {}
      : { role: "log" as const, "aria-live": ariaLive };

  return (
    <div
      className={cn(
        "text-text dark:text-dark-text flex min-h-0 min-w-0 flex-1 flex-col",
        className,
      )}
    >
      <div
        ref={containerRef}
        data-chat-thread=""
        className={cn(
          "flex-1 space-y-6 overflow-auto p-6",
          scrollClassName,
        )}
        {...liveProps}
      >
        {children}
        <div ref={endRef} className="h-4 shrink-0" aria-hidden />
      </div>
    </div>
  );
}
