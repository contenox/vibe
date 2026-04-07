import React from "react";
import type { Components } from "react-markdown";
import { cn } from "../../utils";

/** Default ReactMarkdown `components` map for assistant/user transcript content (code blocks, blockquotes). */
export const chatTranscriptMarkdownComponents: Components = {
  code: ((props: {
    inline?: boolean;
    className?: string;
    children?: React.ReactNode;
    [key: string]: unknown;
  }) => {
    const { inline, className, children, ...rest } = props;
    const isInline = inline ?? false;

    if (!isInline) {
      return (
        <pre className="bg-surface-200 text-text dark:bg-dark-surface-700 dark:text-dark-text overflow-auto rounded-lg p-3 text-sm sm:p-4">
          <code className={className} {...rest}>
            {children}
          </code>
        </pre>
      );
    }

    return (
      <code
        className="bg-surface-200 text-text dark:bg-dark-surface-700 dark:text-dark-text rounded px-1.5 py-0.5 font-mono text-xs"
        {...rest}
      >
        {children}
      </code>
    );
  }) as Components["code"],

  blockquote: ({ children, ...props }: React.ComponentPropsWithoutRef<"blockquote">) => (
    <blockquote
      className="border-primary-400 dark:border-dark-primary-500 bg-surface-50/50 text-text dark:bg-dark-surface-300/20 dark:text-dark-text my-3 rounded-r-lg border-l-4 py-2 pl-4"
      {...props}
    >
      {children}
    </blockquote>
  ),
};

export function ChatStreamThinkingBox({
  className,
  children,
}: {
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div
      className={cn(
        "border-primary-300/50 bg-surface-50/60 dark:border-dark-primary-600/40 dark:bg-dark-surface-300/30 text-text-muted dark:text-dark-text-muted mb-3 max-h-48 overflow-auto rounded-md border px-3 py-2 font-mono text-xs whitespace-pre-wrap",
        className,
      )}
    >
      {children}
    </div>
  );
}

export function ChatTranscriptStreamingPlaceholder({ children }: { children: React.ReactNode }) {
  return <p className="text-text-muted dark:text-dark-text-muted text-sm italic">{children}</p>;
}

export function ChatStreamingCaret({ className }: { className?: string }) {
  return (
    <span
      className={cn(
        "bg-primary-500 ml-0.5 inline-block h-3 w-1.5 animate-pulse rounded-sm align-middle",
        className,
      )}
      aria-hidden
    />
  );
}
