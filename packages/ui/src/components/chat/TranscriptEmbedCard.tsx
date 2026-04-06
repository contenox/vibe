import React from "react";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
  useCollapsibleContext,
} from "../Collapsible";
import { cn } from "../../utils";
import { Span } from "../Typography";

function ToggleGlyph() {
  const { open } = useCollapsibleContext();
  return (
    <span aria-hidden className="text-text-muted dark:text-dark-text-muted shrink-0 tabular-nums">
      {open ? "−" : "+"}
    </span>
  );
}

export type TranscriptEmbedCardProps = {
  title: React.ReactNode;
  /** Optional right side of the header (e.g. caption). */
  headerRight?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  /** Initial open state; default false so tall embeds stay collapsed in the thread. */
  defaultOpen?: boolean;
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
};

/**
 * Collapsible block for non-message content inside a chat transcript (plans, previews).
 * Uses the same surface vocabulary as workbench panels, not marketing Card chrome.
 */
export function TranscriptEmbedCard({
  title,
  headerRight,
  children,
  className,
  defaultOpen = false,
  open,
  onOpenChange,
}: TranscriptEmbedCardProps) {
  return (
    <Collapsible
      open={open}
      onOpenChange={onOpenChange}
      defaultOpen={defaultOpen}
      className={cn(
        "border-border bg-surface-50 dark:bg-dark-surface-200 w-full overflow-hidden rounded-lg border",
        className,
      )}>
      <CollapsibleTrigger
        className={cn(
          "h-auto min-h-0 w-full justify-between gap-2 rounded-none border-0 bg-transparent px-3 py-2.5 font-normal shadow-none ring-0 ring-offset-0",
          "hover:bg-surface-100/80 dark:hover:bg-dark-surface-100/80",
        )}
      >
        <span className="flex min-w-0 flex-1 items-center justify-between gap-2 text-left">
          <Span variant="body" className="truncate font-medium">
            {title}
          </Span>
          {headerRight ? (
            <Span variant="muted" className="shrink-0 text-xs">
              {headerRight}
            </Span>
          ) : null}
        </span>
        <ToggleGlyph />
      </CollapsibleTrigger>
      <CollapsibleContent className="border-border border-t px-2 pb-2 pt-2">
        {children}
      </CollapsibleContent>
    </Collapsible>
  );
}
