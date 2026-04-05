import { ReactNode, useState, useId } from "react";
import { cn } from "../utils";

type TooltipProps = {
  content: string;
  children: ReactNode;
  position?: "top" | "bottom" | "left" | "right";
  className?: string;
};

export function Tooltip({
  content,
  children,
  position = "top",
  className,
}: TooltipProps) {
  const [show, setShow] = useState(false);
  const tooltipId = useId();

  return (
    <div className="relative inline-block">
      <div
        onMouseEnter={() => setShow(true)}
        onMouseLeave={() => setShow(false)}
        onFocus={() => setShow(true)}
        onBlur={() => setShow(false)}
        aria-describedby={show ? tooltipId : undefined}
      >
        {children}
      </div>
      {show && (
        <div
          id={tooltipId}
          role="tooltip"
          className={cn(
            "absolute z-50 rounded-md px-2 py-1 text-sm",
            "bg-secondary-800 text-surface-50 dark:bg-dark-surface-200 dark:text-dark-text",
            "animate-in fade-in-0 zoom-in-95",
            {
              "bottom-full left-1/2 mb-2 -translate-x-1/2": position === "top",
              "top-full left-1/2 mt-2 -translate-x-1/2": position === "bottom",
              "top-1/2 right-full mr-2 -translate-y-1/2": position === "left",
              "top-1/2 left-full ml-2 -translate-y-1/2": position === "right",
            },
            className,
          )}
        >
          {content}
        </div>
      )}
    </div>
  );
}
