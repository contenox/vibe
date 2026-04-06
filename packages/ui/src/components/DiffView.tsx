import { forwardRef } from "react";
import { cn } from "../utils";

export interface DiffLine {
  type: "add" | "remove" | "context";
  content: string;
  oldLineNumber?: number;
  newLineNumber?: number;
}

export interface DiffViewProps extends React.HTMLAttributes<HTMLDivElement> {
  /** The file path shown in the header. */
  filePath: string;
  /** Parsed diff lines. */
  lines: DiffLine[];
  /** Optional language hint (for future syntax highlighting). */
  language?: string;
}

const lineTypeStyles: Record<DiffLine["type"], string> = {
  add: "bg-success-50 dark:bg-dark-success-50 text-success-800 dark:text-dark-success-800",
  remove: "bg-error-50 dark:bg-dark-error-50 text-error-800 dark:text-dark-error-800",
  context: "text-text dark:text-dark-text",
};

const gutterStyles: Record<DiffLine["type"], string> = {
  add: "bg-success-100 dark:bg-dark-success-100 text-success-600 dark:text-dark-success-600",
  remove: "bg-error-100 dark:bg-dark-error-100 text-error-600 dark:text-dark-error-600",
  context:
    "bg-surface-100 dark:bg-dark-surface-300 text-text-muted dark:text-dark-text-muted",
};

const prefixChar: Record<DiffLine["type"], string> = {
  add: "+",
  remove: "-",
  context: " ",
};

export const DiffView = forwardRef<HTMLDivElement, DiffViewProps>(
  function DiffView({ className, filePath, lines, language, ...props }, ref) {
    return (
      <div
        ref={ref}
        className={cn(
          "overflow-hidden rounded-lg border",
          "border-surface-200 dark:border-dark-surface-500",
          "text-sm",
          className,
        )}
        {...props}
      >
        {/* File header */}
        <div
          className={cn(
            "flex items-center gap-2 border-b px-3 py-1.5",
            "bg-surface-100 dark:bg-dark-surface-300",
            "border-surface-200 dark:border-dark-surface-500",
          )}
        >
          <span className="font-mono text-xs font-medium text-text dark:text-dark-text">
            {filePath}
          </span>
          {language && (
            <span className="text-xs text-text-muted dark:text-dark-text-muted">
              {language}
            </span>
          )}
        </div>

        {/* Lines */}
        <div className="overflow-x-auto">
          <table className="w-full border-collapse font-mono text-xs leading-5">
            <tbody>
              {lines.map((line, i) => (
                <tr key={i} className={lineTypeStyles[line.type]}>
                  {/* Old line number */}
                  <td
                    className={cn(
                      "w-10 select-none px-2 text-right align-top",
                      gutterStyles[line.type],
                    )}
                  >
                    {line.type !== "add" ? (line.oldLineNumber ?? "") : ""}
                  </td>
                  {/* New line number */}
                  <td
                    className={cn(
                      "w-10 select-none px-2 text-right align-top",
                      gutterStyles[line.type],
                    )}
                  >
                    {line.type !== "remove"
                      ? (line.newLineNumber ?? "")
                      : ""}
                  </td>
                  {/* Prefix */}
                  <td className="w-4 select-none px-1 text-center align-top">
                    {prefixChar[line.type]}
                  </td>
                  {/* Content */}
                  <td className="whitespace-pre-wrap break-all px-2 align-top">
                    {line.content}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    );
  },
);
