import type { ReactNode } from "react";
import { cn } from "../../utils";
import { Button } from "../Button";
import { Panel } from "../Panel";
import { Span } from "../Typography";
import { Spinner } from "../Spinner";

export type ChatProcessingBarProps = {
  label: ReactNode;
  onStop?: () => void;
  stopLabel?: ReactNode;
  className?: string;
};

export function ChatProcessingBar({
  label,
  onStop,
  stopLabel = "Stop",
  className,
}: ChatProcessingBarProps) {
  return (
    <Panel
      className={cn(
        "bg-surface-100 dark:bg-dark-surface-200 text-text dark:text-dark-text mx-4 mt-4 shrink-0",
        className,
      )}
    >
      <div className="flex items-center gap-3">
        <Spinner size="sm" />
        <Span variant="body">{label}</Span>
        <div className="flex-1" />
        {onStop != null && (
          <Button size="sm" variant="outline" onClick={onStop} type="button">
            {stopLabel}
          </Button>
        )}
      </div>
    </Panel>
  );
}
