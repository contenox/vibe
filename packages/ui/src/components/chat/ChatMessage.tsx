import React, { useState } from "react";
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
  switch (role) {
    case "user":
      return "bg-primary-600 text-white";
    case "system":
      return "bg-accent-600 text-white";
    case "tool":
      return "bg-secondary-600 text-white";
    default:
      return "bg-secondary-600 text-white";
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
  const isSystem = role === "system";
  const isTool = role === "tool";
  if (isUser) {
    return "bg-primary-100 text-text dark:bg-dark-primary-700 dark:text-dark-text";
  }
  if (isSystem || isTool) {
    return "bg-accent-100 text-text dark:bg-dark-accent-700 dark:text-dark-text";
  }
  return "bg-surface-100 text-text dark:bg-dark-surface-500 dark:text-dark-text";
}

/** Left border + surface for transcript / workbench layout */
function transcriptBlockClass(role: ChatMessageBaseProps["role"]): string {
  switch (role) {
    case "user":
      return "border-primary-600 bg-primary-50/70 text-text dark:border-primary-500 dark:bg-dark-primary-900/30 dark:text-dark-text";
    case "system":
      return "border-accent-600 bg-accent-50/80 text-text dark:border-accent-500 dark:bg-dark-accent-900/25 dark:text-dark-text";
    case "tool":
      return "border-secondary-600 bg-secondary-50/80 text-text dark:border-secondary-500 dark:bg-dark-surface-600/40 dark:text-dark-text";
    default:
      return "border-secondary-500 bg-surface-100/90 text-text dark:border-dark-secondary-500 dark:bg-dark-surface-600/35 dark:text-dark-text";
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
  highlightLatest = true,
  defaultOpen = true,
  onOpenChange,
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

  const bubbleRing =
    isLatest && highlightLatest
      ? "ring-2 ring-primary-300 dark:ring-dark-primary-400"
      : "";

  const transcriptRing =
    isLatest && highlightLatest
      ? "ring-2 ring-primary-300/70 dark:ring-dark-primary-500/60"
      : "";

  const handleOpenChange = (next: boolean) => {
    setOpen(next);
    onOpenChange?.(next);
  };

  const handleCopy = async () => {
    if (!copyText) return;
    const ok = await copyTextToClipboard(copyText);
    if (ok) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
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
          open={open}
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
          </div>

          <CollapsibleContent>
            <div
              className={cn(
                "rounded-r-lg border-l-4 py-3 pr-3 pl-4",
                transcriptBlockClass(role),
                transcriptRing,
              )}
            >
              <div className="prose prose-sm dark:prose-invert max-w-none min-w-0">
                {children}
              </div>

              {error != null && (
                <Panel className="bg-error-50 dark:bg-dark-error-600/30 text-error-800 dark:text-dark-text mt-3">
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

            <div className="mt-1 flex flex-wrap items-center gap-2">
              {copyText != null && (
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-6 text-xs"
                  onClick={() => void handleCopy()}
                  aria-live="polite"
                  type="button"
                  aria-label={
                    copied
                      ? (copiedLabel != null ? String(copiedLabel) : "Copied")
                      : (copyLabel != null ? String(copyLabel) : "Copy")
                  }
                >
                  {copied
                    ? (copiedLabel ?? "Copied!")
                    : (copyLabel ?? "Copy")}
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
        open={open}
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
              <div className="prose prose-sm dark:prose-invert max-w-none">
                {children}
              </div>

              {error != null && (
                <Panel className="bg-error-50 dark:bg-dark-error-600/30 text-error-800 dark:text-dark-text mt-3">
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
                "mt-1 flex items-center gap-2 opacity-0 transition-opacity group-hover:opacity-100",
                isUser && "flex-row-reverse",
              )}
            >
              {copyText != null && (
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-6 text-xs"
                  onClick={() => void handleCopy()}
                  aria-live="polite"
                  type="button"
                  aria-label={
                    copied
                      ? (copiedLabel != null ? String(copiedLabel) : "Copied")
                      : (copyLabel != null ? String(copyLabel) : "Copy")
                  }
                >
                  {copied
                    ? (copiedLabel ?? "Copied!")
                    : (copyLabel ?? "Copy")}
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
