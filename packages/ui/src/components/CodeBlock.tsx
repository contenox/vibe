import { forwardRef } from "react";
import { cn } from "../utils";

export const CodeBlock = forwardRef<HTMLPreElement, React.HTMLAttributes<HTMLPreElement>>(
  function CodeBlock({ className, ...props }, ref) {
    return (
      <pre
        ref={ref}
        className={cn(
          "font-mono text-xs leading-relaxed",
          "text-text dark:text-dark-text",
          "overflow-auto whitespace-pre",
          className,
        )}
        {...props}
      />
    );
  },
);
