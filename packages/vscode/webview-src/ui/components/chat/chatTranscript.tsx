import { Check, Copy as CopyIcon, FileDiff, TextCursorInput } from "lucide-react";
import React from "react";
import type { Components } from "react-markdown";
import { cn } from "../../utils";
import { copyTextToClipboard } from "./clipboard";

// Phase 4 "code out of the panel" (vscode-implementation-plan.md §5): every
// rendered code block gets Copy / Insert at cursor / Apply to file. Pure
// client-side -- Copy uses the clipboard directly, Insert/Apply call back
// into the host over the existing wire (ChatSurface.tsx wires these from
// BeamChatClient.insertAtCursor/applyCodeBlock). No protocol work.
export type CodeBlockActionHandlers = {
  onInsert?: (code: string, language?: string) => void;
  onApply?: (code: string, language?: string, hintPath?: string) => void;
};

function firstElementChild(children: React.ReactNode): React.ReactElement<any> | undefined {
  const found = React.Children.toArray(children).find((child) => React.isValidElement(child));
  return React.isValidElement(found) ? (found as React.ReactElement<any>) : undefined;
}

function codeTextFromChildren(children: React.ReactNode): string {
  return React.Children.toArray(children)
    .map((child) => (typeof child === "string" ? child : ""))
    .join("");
}

// Best-effort "the block names a file" detection (Phase 4's diff-first Apply
// requirement): a leading comment naming a path, e.g. `// src/foo.ts` or
// `# src/foo.py`. The coding chain's prompt isn't ours to change here (no Go
// work in this pass), so this is a heuristic, not a guarantee -- when it
// finds nothing, the host falls back to the active editor / a manual prompt
// rather than silently doing nothing.
function detectFilePathHint(codeText: string): string | undefined {
  const firstLine = codeText.split("\n", 1)[0] ?? "";
  const match = /^\s*(?:\/\/|#|--|;;|<!--)\s*([\w./-]+\.[A-Za-z0-9]+)\s*(?:-->)?\s*$/.exec(firstLine);
  return match?.[1];
}

function CodeBlockToolbar({
  code,
  language,
  handlers,
}: {
  code: string;
  language?: string;
  handlers?: CodeBlockActionHandlers;
}) {
  const [copied, setCopied] = React.useState(false);
  const buttonClass =
    "inline-flex items-center gap-1 rounded px-2 py-1 hover:bg-surface-200 dark:hover:bg-dark-surface-500 text-text-muted dark:text-dark-text-muted hover:text-text dark:hover:text-dark-text";

  const handleCopy = async () => {
    const ok = await copyTextToClipboard(code);
    if (ok) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    }
  };

  return (
    <div
      className="border-surface-300 dark:border-dark-surface-600 bg-surface-100/80 dark:bg-dark-surface-300/50 flex items-center justify-end gap-1 rounded-t-lg border px-2 py-1 text-xs"
      contentEditable={false}>
      {/* Distinct aria-labels (not just visible "Copy"/"Insert"/"Apply") --
          the per-message Copy button (ChatMessage's copyText prop) already
          uses the accessible name "Copy"/"Copied"; without these, a code
          block's toolbar button and the message-level one are
          indistinguishable to assistive tech and to role-based test
          selectors alike. */}
      <button
        aria-label={copied ? "Copied code" : "Copy code"}
        className={buttonClass}
        onClick={() => void handleCopy()}
        type="button">
        {copied ? <Check className="h-3 w-3" aria-hidden /> : <CopyIcon className="h-3 w-3" aria-hidden />}
        {copied ? "Copied" : "Copy"}
      </button>
      {handlers?.onInsert ? (
        <button
          aria-label="Insert code at cursor"
          className={buttonClass}
          onClick={() => handlers.onInsert?.(code, language)}
          type="button">
          <TextCursorInput className="h-3 w-3" aria-hidden />
          Insert
        </button>
      ) : null}
      {handlers?.onApply ? (
        <button
          aria-label="Apply code to file"
          className={buttonClass}
          onClick={() => handlers.onApply?.(code, language, detectFilePathHint(code))}
          type="button">
          <FileDiff className="h-3 w-3" aria-hidden />
          Apply
        </button>
      ) : null}
    </div>
  );
}

/** Builds the ReactMarkdown `components` map for assistant/user transcript content, with code-block actions wired to `handlers`. */
export function createChatTranscriptMarkdownComponents(handlers?: CodeBlockActionHandlers): Components {
  return {
    pre: (props: React.ComponentPropsWithoutRef<"pre">) => {
      const codeElement = firstElementChild(props.children);
      const codeText = codeElement ? codeTextFromChildren(codeElement.props?.children).replace(/\n$/, "") : "";
      const match = /language-(\w+)/.exec(codeElement?.props?.className || "");
      return (
        <div className="my-2">
          <CodeBlockToolbar code={codeText} handlers={handlers} language={match?.[1]} />
          <pre
            className="bg-surface-200 text-text dark:bg-dark-surface-700 dark:text-dark-text overflow-auto rounded-b-lg p-3 text-sm sm:p-4"
            {...props}
          />
        </div>
      );
    },

    code: (props: React.ComponentPropsWithoutRef<"code"> & { node?: any }) => {
      const { className, children, node, ...rest } = props;
      const match = /language-(\w+)/.exec(className || "");

      // If it has a language class, or its parent is a pre tag, it's a block code.
      // react-markdown will wrap this code block in the pre component above.
      if (match || (node && node.parent && node.parent.type === "element" && node.parent.tagName === "pre")) {
        return (
          <code className={className} {...rest}>
            {children}
          </code>
        );
      }

      // Inline code styling
      return (
        <code
          className={cn(
            "bg-surface-200 text-text dark:bg-dark-surface-700 dark:text-dark-text rounded px-1.5 py-0.5 font-mono text-xs",
            className
          )}
          {...rest}
        >
          {children}
        </code>
      );
    },

    // Markdown-embedded images must degrade nicely inside a chat column: never
    // overflow the bubble, keep the transcript's rounded/bordered look.
    img: ({ className, ...props }: React.ComponentPropsWithoutRef<"img">) => (
      <img
        loading="lazy"
        className={cn(
          "border-surface-200 dark:border-dark-surface-700 my-2 h-auto max-w-full rounded-lg border",
          className,
        )}
        {...props}
      />
    ),

    blockquote: ({ children, ...props }: React.ComponentPropsWithoutRef<"blockquote">) => (
      <blockquote
        className="border-primary-400 dark:border-dark-primary-500 bg-surface-50/50 text-text dark:bg-dark-surface-300/20 dark:text-dark-text rounded-r-lg border-l-4 py-2 pl-4"
        {...props}
      >
        {children}
      </blockquote>
    ),
  };
}

/** Default components map, no code-block Insert/Apply handlers (Copy still works -- it needs no host round trip). */
export const chatTranscriptMarkdownComponents: Components = createChatTranscriptMarkdownComponents();

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
        "border-primary-300/50 bg-surface-50/60 dark:border-dark-primary-600/40 dark:bg-dark-surface-300/30 text-text-muted dark:text-dark-text-muted max-h-48 overflow-auto rounded-md border px-3 py-2 font-mono text-xs whitespace-pre-wrap",
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
