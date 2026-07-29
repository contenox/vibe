import { Check } from "lucide-react";
import { useState } from "react";
import { cn } from "../../utils";
import { Badge } from "../Badge";
import { Button } from "../Button";
import { Card } from "../Card";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "../Collapsible";
import { Panel } from "../Panel";
import { Span } from "../Typography";
import { Tooltip } from "../Tooltip";
import { copyTextToClipboard } from "./clipboard";
import type { ChatMessageBaseProps } from "./types";

function defaultAvatarLetter(role: ChatMessageBaseProps["role"]): string {
  switch (role) {
    case "user":
      return "U";
    case "system":
      return "S";
    case "tool":
      return "T";
    default:
      return "A";
  }
}

function avatarRingClass(role: ChatMessageBaseProps["role"]): string {
  // Light mode uses the soft -100/-800 pairing (same convention as Badge) —
  // a solid -600 disc reads far too heavy next to light bubbles.
  switch (role) {
    case "user":
      return "bg-primary-100 text-primary-800 dark:bg-dark-primary-600 dark:text-dark-text-inverted";
    case "system":
      return "bg-accent-100 text-accent-800 dark:bg-dark-accent-600 dark:text-dark-text";
    case "tool":
      return "bg-secondary-100 text-secondary-800 dark:bg-dark-secondary-600 dark:text-dark-text";
    default:
      return "bg-secondary-100 text-secondary-800 dark:bg-dark-secondary-600 dark:text-dark-text";
  }
}

function roleBadgeVariant(
  role: ChatMessageBaseProps["role"],
): "primary" | "accent" | "secondary" {
  if (role === "user") return "primary";
  if (role === "system") return "accent";
  return "secondary";
}

function bubbleBgClass(role: ChatMessageBaseProps["role"]): string {
  const isUser = role === "user";
  if (isUser) {
    return "bg-surface-100 text-text dark:bg-dark-surface-300 dark:text-dark-text";
  }
  return "bg-surface-50 text-text dark:bg-dark-surface-200 dark:text-dark-text";
}

function transcriptBlockClass(role: ChatMessageBaseProps["role"]): string {
  switch (role) {
    case "user":
      return "border-l-primary-500 bg-surface-50 text-text shadow-sm dark:border-l-dark-primary-500 dark:bg-dark-surface-300/40 dark:text-dark-text";
    case "system":
      return "border-l-primary-400 bg-surface-50/70 text-text shadow-sm dark:border-l-dark-primary-600 dark:bg-dark-surface-300/30 dark:text-dark-text";
    case "tool":
      return "border-l-secondary-500 bg-surface-50/70 text-text shadow-sm dark:border-l-dark-surface-500 dark:bg-dark-surface-300/30 dark:text-dark-text";
    default:
      return "border-l-secondary-500 bg-surface-50/70 text-text shadow-sm dark:border-l-dark-surface-500 dark:bg-dark-surface-300/30 dark:text-dark-text";
  }
}

export function ChatMessage({
  role,
  roleLabel,
  children,
  avatar,
  timestamp,
  timestampTooltip,
  isLatest = false,
  latestLabel,
  statusBadge,
  highlightLatest = true,
  defaultOpen = true,
  onOpenChange,
  collapsible = true,
  error,
  onRetry,
  retryLabel,
  collapseToggleLabel,
  secondaryActions,
  copyText,
  copyLabel,
  copiedLabel,
  className,
  "aria-label": ariaLabel,
  appearance = "bubble",
}: ChatMessageBaseProps) {
  const [open, setOpen] = useState(defaultOpen);
  const [copied, setCopied] = useState(false);
  const isUser = role === "user";
  // Non-collapsible transcripts opt out of the per-message Hide/Show toggle
  // entirely (only thought blocks / tool detail collapse there) — force the
  // body open and skip rendering the trigger below.
  const effectiveOpen = collapsible ? open : true;

  const bubbleRing =
    isLatest && highlightLatest
      ? "ring-2 ring-surface-300 dark:ring-dark-surface-500"
      : "";

  const transcriptRing =
    isLatest && highlightLatest
      ? "ring-2 ring-surface-300/70 dark:ring-dark-surface-500/60"
      : "";

  const handleOpenChange = (next: boolean) => {
    setOpen(next);
    onOpenChange?.(next);
  };

  const handleCopy = async (button?: HTMLButtonElement | null) => {
    if (!copyText) return;
    const ok = await copyTextToClipboard(copyText);
    if (ok) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
      // Root cause of the ring "sticking" after a mouse click: focus never
      // leaves the button once clicked, so its focus ring stays visible
      // indefinitely. Blurring on successful copy clears it without
      // affecting keyboard users (a subsequent Tab/Shift+Tab still moves
      // focus normally).
      button?.blur();
    }
  };

  const collapseLabels = collapseToggleLabel ?? {
    open: "Hide",
    closed: "Show",
  };

  const ts = timestampTooltip ? (
    <Tooltip content={timestampTooltip}>
      <Span
        variant="muted"
        className="text-secondary-600 dark:text-dark-text-muted text-xs"
      >
        {timestamp}
      </Span>
    </Tooltip>
  ) : (
    <Span
      variant="muted"
      className="text-secondary-600 dark:text-dark-text-muted text-xs"
    >
      {timestamp}
    </Span>
  );

  const articleLabel =
    ariaLabel ?? (typeof roleLabel === "string" ? roleLabel : "message");

  if (appearance === "transcript") {
    return (
      <article aria-label={articleLabel} className={cn("group", className)}>
        <Collapsible
          open={effectiveOpen}
          onOpenChange={handleOpenChange}
          className="flex flex-col gap-1.5"
        >
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant={roleBadgeVariant(role)} size="sm">
              {roleLabel}
            </Badge>
            {timestamp != null && ts}
            {isLatest && latestLabel != null && (
              <Badge variant="success" size="sm">
                {latestLabel}
              </Badge>
            )}
            {statusBadge}
            {collapsible && (
              <CollapsibleTrigger asChild>
                <Button
                  variant="ghost"
                  size="xs"
                  className="h-6 px-2 text-xs"
                  type="button"
                >
                  {open ? collapseLabels.open : collapseLabels.closed}
                </Button>
              </CollapsibleTrigger>
            )}
          </div>

          <CollapsibleContent>
            <div
              className={cn(
                "rounded-r-lg border border-l-4 border-surface-200 dark:border-dark-surface-600 py-3 pr-3 pl-4",
                transcriptBlockClass(role),
                transcriptRing,
              )}
            >
              <div className="prose prose-compact dark:prose-invert max-w-none min-w-0">
                {children}
              </div>

              {error != null && (
                <Panel className="bg-error-50 dark:bg-dark-error-600/30 text-error-800 dark:text-dark-text">
                  <div className="flex items-center justify-between gap-2">
                    <Span className="text-sm">{error}</Span>
                    {onRetry != null && (
                      <Button variant="ghost" size="sm" onClick={onRetry}>
                        {retryLabel ?? "Retry"}
                      </Button>
                    )}
                  </div>
                </Panel>
              )}
            </div>

            <div className="flex flex-wrap items-center gap-2 p-0.5">
              {copyText != null && (
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-6 text-xs"
                  onClick={e => void handleCopy(e.currentTarget)}
                  aria-live="polite"
                  type="button"
                  aria-label={
                    copied
                      ? copiedLabel != null
                        ? String(copiedLabel)
                        : "Copied"
                      : copyLabel != null
                        ? String(copyLabel)
                        : "Copy"
                  }
                >
                  {copied ? (
                    <span className="flex items-center gap-1">
                      <Check className="h-3.5 w-3.5" aria-hidden />
                      {copiedLabel ?? "Copied!"}
                    </span>
                  ) : (
                    (copyLabel ?? "Copy")
                  )}
                </Button>
              )}
              {secondaryActions}
            </div>
          </CollapsibleContent>
        </Collapsible>
      </article>
    );
  }

  return (
    <article aria-label={articleLabel} className={cn("group", className)}>
      <Collapsible
        open={effectiveOpen}
        onOpenChange={handleOpenChange}
        className={cn("flex gap-3", isUser && "flex-row-reverse")}
      >
        <div
          className={cn(
            "flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-xs font-semibold",
            avatarRingClass(role),
          )}
          aria-hidden
        >
          {avatar ?? defaultAvatarLetter(role)}
        </div>

        <div
          className={cn(
            "flex max-w-[85%] flex-col gap-2",
            isUser && "items-end",
          )}
        >
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant={roleBadgeVariant(role)} size="sm">
              {roleLabel}
            </Badge>
            {timestamp != null && ts}
            {isLatest && latestLabel != null && (
              <Badge variant="success" size="sm">
                {latestLabel}
              </Badge>
            )}
            {statusBadge}
            {collapsible && (
              <CollapsibleTrigger asChild>
                <Button
                  variant="ghost"
                  size="xs"
                  className="h-6 px-2 text-xs"
                  type="button"
                >
                  {open ? collapseLabels.open : collapseLabels.closed}
                </Button>
              </CollapsibleTrigger>
            )}
          </div>

          <CollapsibleContent>
            <Card
              variant="surface"
              className={cn(
                "border-surface-200 dark:border-dark-surface-600 rounded-xl border p-4 shadow-sm group-hover:shadow-md",
                bubbleBgClass(role),
                bubbleRing,
              )}
            >
              <div className="prose prose-compact dark:prose-invert max-w-none">
                {children}
              </div>

              {error != null && (
                <Panel className="bg-error-50 dark:bg-dark-error-600/30 text-error-800 dark:text-dark-text">
                  <div className="flex items-center justify-between gap-2">
                    <Span className="text-sm">{error}</Span>
                    {onRetry != null && (
                      <Button variant="ghost" size="sm" onClick={onRetry}>
                        {retryLabel ?? "Retry"}
                      </Button>
                    )}
                  </div>
                </Panel>
              )}
            </Card>

            <div
              className={cn(
                "flex items-center gap-2 p-0.5 opacity-0 transition-opacity group-hover:opacity-100",
                isUser && "flex-row-reverse",
              )}
            >
              {copyText != null && (
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-6 text-xs"
                  onClick={e => void handleCopy(e.currentTarget)}
                  aria-live="polite"
                  type="button"
                  aria-label={
                    copied
                      ? copiedLabel != null
                        ? String(copiedLabel)
                        : "Copied"
                      : copyLabel != null
                        ? String(copyLabel)
                        : "Copy"
                  }
                >
                  {copied ? (
                    <span className="flex items-center gap-1">
                      <Check className="h-3.5 w-3.5" aria-hidden />
                      {copiedLabel ?? "Copied!"}
                    </span>
                  ) : (
                    (copyLabel ?? "Copy")
                  )}
                </Button>
              )}
              {secondaryActions}
            </div>
          </CollapsibleContent>
        </div>
      </Collapsible>
    </article>
  );
}
