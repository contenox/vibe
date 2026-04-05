import React, { type FormEvent, useRef, useState } from "react";
import { cn } from "../../utils";
import { Badge } from "../Badge";
import { Button } from "../Button";
import { Panel } from "../Panel";
import { Textarea } from "../TextArea";
import { H2 } from "../Typography";
import { Tooltip } from "../Tooltip";
import { Spinner } from "../Spinner";

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
  variant?: "default" | "compact";
  maxLength?: number;
  showCharCount?: boolean;
  charCountFormatter?: (length: number, max: number) => string;
  /** When false, submit is disabled regardless of value */
  canSubmit?: boolean;
  footerStart?: React.ReactNode;
  /** When set, wraps the character counter in a Tooltip */
  charCountTooltip?: string;
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
  maxLength = 4000,
  showCharCount = true,
  charCountFormatter = (len, max) => `${len}/${max}`,
  canSubmit = true,
  footerStart,
  charCountTooltip,
  textareaProps,
}: ChatComposerProps) {
  const formRef = useRef<HTMLFormElement>(null);
  const [isFocused, setIsFocused] = useState(false);
  const {
    onKeyDown: onKeyDownProp,
    className: textareaClassName,
    ...restTextareaProps
  } = textareaProps ?? {};

  const submitDisabled =
    disabled || isPending || !value.trim() || !canSubmit;

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    onKeyDownProp?.(e);
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      if (!submitDisabled) {
        formRef.current?.requestSubmit();
      }
    }
  };

  const countStr = charCountFormatter(value.length, maxLength);
  const countWarning = value.length > maxLength * 0.875;

  const textareaBlock = (
    <div className="relative flex-1">
      <Textarea
        {...restTextareaProps}
        placeholder={placeholder}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onFocus={() => setIsFocused(true)}
        onBlur={() => setIsFocused(false)}
        required
        disabled={disabled}
        className={cn(
          baseTextarea,
          variant === "compact"
            ? "resize-vertical min-h-[60px]"
            : "resize-vertical min-h-[80px]",
          textareaClassName,
        )}
        maxLength={maxLength}
        onKeyDown={handleKeyDown}
      />
      {showCharCount && (
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

  const submitButton = (compactHeight?: boolean) => (
    <Button
      type="submit"
      variant="primary"
      disabled={submitDisabled}
      size="lg"
      className={compactHeight ? "h-[60px]" : undefined}
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

  return (
    <Panel
      variant="default"
      className={cn(
        "transition-all duration-200",
        isFocused && "ring-primary-100 dark:ring-dark-primary-500 ring-2",
        className,
      )}
    >
      <form ref={formRef} onSubmit={handleFormSubmit} className="space-y-6">
        {title != null && title !== "" && (
          <H2 className="text-text dark:text-dark-text text-2xl font-semibold">
            {title}
          </H2>
        )}

        <div className="space-y-4">
          <div className="space-y-3">
            <div className="flex gap-2">
              {textareaBlock}
            </div>

            <div className="flex items-center justify-between">
              <div className="flex flex-wrap items-center gap-2">
                {footerStart}
              </div>
              {submitButton()}
            </div>
          </div>
        </div>
      </form>
    </Panel>
  );
}
