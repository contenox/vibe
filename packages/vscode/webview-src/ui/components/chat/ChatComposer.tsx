import React, { type FormEvent, useRef, useState } from "react";
import { cn } from "../../utils";
import { Badge } from "../Badge";
import { Button } from "../Button";
import { Panel } from "../Panel";
import { Textarea } from "../TextArea";
import { H2 } from "../Typography";
import { Tooltip } from "../Tooltip";
import { Spinner } from "../Spinner";
import {
  DEFAULT_COMPOSER_SOFT_MAX,
  isComposerCharCountWarning,
  isOverComposerSoftMax,
} from "./composerSoftLimit";

export type ChatComposerProps = {
  value: string;
  onChange: (value: string) => void;
  onSubmit: (e: FormEvent) => void;
  placeholder?: string;
  isPending?: boolean;
  disabled?: boolean;
  submitLabel?: React.ReactNode;
  pendingLabel?: React.ReactNode;
  title?: string;
  className?: string;
  variant?: "default" | "compact" | "workbench";
  /** Outer shell: panel (default) or plain border-top (workbench). */
  shell?: "panel" | "plain";
  /**
   * Soft reference for character count and warning styling only — input is not hard-capped.
   * @default 131072 (128 KiB)
   */
  softMax?: number;
  /** @deprecated Use softMax — if set, treated as softMax for backward compatibility. */
  maxLength?: number;
  showCharCount?: boolean;
  charCountFormatter?: (length: number, softMax: number) => string;
  /** When false, submit is disabled regardless of value */
  canSubmit?: boolean;
  /** When true, an empty message can be submitted (e.g. build mode). */
  allowEmptyMessage?: boolean;
  footerStart?: React.ReactNode;
  /** Extra controls after footerStart (e.g. expand editor). */
  footerEnd?: React.ReactNode;
  /**
   * Pending-attachment strip rendered above the textarea, inside the composer
   * shell (e.g. removable image thumbnails). Layout/styling belongs to the
   * caller — this is a plain slot, like footerStart/footerEnd.
   */
  attachments?: React.ReactNode;
  /** When set, wraps the character counter in a Tooltip */
  charCountTooltip?: string;
  /** Shown under the composer when length exceeds softMax (e.g. model context hint). */
  softLimitExceededNote?: string;
  textareaProps?: Omit<
    React.TextareaHTMLAttributes<HTMLTextAreaElement>,
    "value" | "onChange"
  >;
};

const baseTextarea =
  "border rounded-md " +
  "bg-surface-50 text-text placeholder:text-secondary-400 border-surface-200 " +
  "dark:bg-dark-surface-600 dark:text-dark-text dark:placeholder:text-dark-secondary-400 dark:border-dark-surface-700";

export function ChatComposer({
  value,
  onChange,
  onSubmit,
  placeholder = "",
  isPending = false,
  disabled = false,
  submitLabel = "Send",
  pendingLabel = "Sending…",
  title,
  className,
  variant = "default",
  shell,
  softMax: softMaxProp,
  maxLength: maxLengthLegacy,
  showCharCount = true,
  charCountFormatter,
  canSubmit = true,
  allowEmptyMessage = false,
  footerStart,
  footerEnd,
  attachments,
  charCountTooltip,
  softLimitExceededNote,
  textareaProps,
}: ChatComposerProps) {
  const softMax = softMaxProp ?? maxLengthLegacy ?? DEFAULT_COMPOSER_SOFT_MAX;
  const formRef = useRef<HTMLFormElement>(null);
  const [isFocused, setIsFocused] = useState(false);
  const {
    onKeyDown: onKeyDownProp,
    className: textareaClassName,
    ...restTextareaProps
  } = textareaProps ?? {};

  const submitDisabled =
    disabled ||
    isPending ||
    (!allowEmptyMessage && !value.trim()) ||
    !canSubmit;

  const effectiveShell: "panel" | "plain" =
    shell ?? (variant === "workbench" ? "plain" : "panel");

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    onKeyDownProp?.(e);
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      if (!submitDisabled) {
        formRef.current?.requestSubmit();
      }
    }
  };

  const countWarning = isComposerCharCountWarning(value.length, softMax);
  const overSoftMax = isOverComposerSoftMax(value.length, softMax);
  // The soft max is a large safety ceiling; spelling it out on every keystroke
  // (e.g. "51/131072") is noise. By default show a bare count while composing and
  // only reveal the ceiling once you approach it. A custom formatter, if provided,
  // is always honored.
  const countStr = charCountFormatter
    ? charCountFormatter(value.length, softMax)
    : countWarning
      ? `${value.length}/${softMax}`
      : `${value.length}`;
  // Hide the badge entirely on an empty composer — "0/131072" signals nothing.
  const showCount = showCharCount && value.length > 0;

  const textareaBlock = (
    <div className="relative flex-1">
      <Textarea
        {...restTextareaProps}
        placeholder={placeholder}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onFocus={() => setIsFocused(true)}
        onBlur={() => setIsFocused(false)}
        required={!allowEmptyMessage}
        disabled={disabled}
        className={cn(
          baseTextarea,
          variant === "compact"
            ? "resize-vertical min-h-[60px]"
            : variant === "workbench"
              ? "min-h-[140px] max-h-[50vh] resize-y sm:min-h-[180px] md:min-h-[200px]"
              : "resize-vertical min-h-[80px]",
          textareaClassName,
        )}
        onKeyDown={handleKeyDown}
      />
      {showCount && (
        <div className="absolute right-2 bottom-2 flex items-center gap-2">
          {charCountTooltip != null ? (
            <Tooltip content={charCountTooltip}>
              <Badge variant={countWarning ? "warning" : "outline"} size="sm">
                {countStr}
              </Badge>
            </Tooltip>
          ) : (
            <Badge variant={countWarning ? "warning" : "outline"} size="sm">
              {countStr}
            </Badge>
          )}
        </div>
      )}
    </div>
  );

  const softNoteBlock =
    overSoftMax && softLimitExceededNote ? (
      <p className="text-text-muted dark:text-dark-secondary-400 text-xs">
        {softLimitExceededNote}
      </p>
    ) : null;

  const submitButton = (compactHeight?: boolean, workbenchTall?: boolean) => (
    <Button
      type="submit"
      variant="primary"
      disabled={submitDisabled}
      size="lg"
      className={cn(
        compactHeight && "h-[60px]",
        workbenchTall && "min-h-[3rem] self-end",
      )}
    >
      {isPending ? (
        <>
          <Spinner size="sm" className="mr-2" />
          {pendingLabel}
        </>
      ) : (
        submitLabel
      )}
    </Button>
  );

  const handleFormSubmit = (e: FormEvent) => {
    e.preventDefault();
    onSubmit(e);
  };

  if (variant === "compact") {
    return (
      <div className={className}>
        {attachments}
        <form
          ref={formRef}
          onSubmit={handleFormSubmit}
          className="flex items-start gap-2"
        >
          {textareaBlock}
          {submitButton(true)}
        </form>
      </div>
    );
  }

  const formInner = (
    <form ref={formRef} onSubmit={handleFormSubmit} className="space-y-6">
      {title != null && title !== "" && variant !== "workbench" && (
        <H2 className="text-text dark:text-dark-text text-2xl font-semibold">
          {title}
        </H2>
      )}

      <div className="space-y-4">
        <div className="space-y-3">
          {attachments}
          <div className="flex gap-2">{textareaBlock}</div>

          <div className="flex items-center justify-between gap-2">
            <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">
              {footerStart}
              {footerEnd}
            </div>
            {submitButton(false, variant === "workbench")}
          </div>
          {softNoteBlock}
        </div>
      </div>
    </form>
  );

  if (variant === "workbench" && effectiveShell === "plain") {
    return (
      <div
        className={cn(
          "border-surface-200 dark:border-dark-surface-600 bg-surface-50/80 dark:bg-dark-surface-100/80 border-t px-3 py-3 transition-all duration-200 sm:px-4",
          isFocused &&
            "ring-primary-100 dark:ring-dark-primary-500 ring-2 ring-inset",
          className,
        )}
      >
        {formInner}
      </div>
    );
  }

  return (
    <Panel
      variant="default"
      className={cn(
        "transition-all duration-200",
        isFocused && "ring-primary-100 dark:ring-dark-primary-500 ring-2",
        className,
      )}
    >
      {formInner}
    </Panel>
  );
}
