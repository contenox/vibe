// components/Checkbox.tsx
import { forwardRef, useEffect, useImperativeHandle, useRef } from "react";
import { cn } from "../utils";
import { Inbox } from "./Inbox";
import { Label } from "./Label";

type CheckboxProps = React.InputHTMLAttributes<HTMLInputElement> & {
  indeterminate?: boolean;
  label?: string;
};

export const Checkbox = forwardRef<HTMLInputElement, CheckboxProps>(
  ({ className, indeterminate, label, ...props }, forwardedRef) => {
    const localRef = useRef<HTMLInputElement>(null);

    useEffect(() => {
      if (forwardedRef) {
        if (typeof forwardedRef === "function") {
          forwardedRef(localRef.current);
        } else {
          (
            forwardedRef as React.MutableRefObject<HTMLInputElement | null>
          ).current = localRef.current;
        }
      }
    }, [forwardedRef]);

    useEffect(() => {
      if (localRef.current) {
        localRef.current.indeterminate = indeterminate ?? false;
      }
    }, [indeterminate]);

    return (
      <Label className="flex items-center gap-2">
        <Inbox
          type="checkbox"
          ref={localRef}
          className={cn(
            "border-secondary-300 text-primary-500 focus:ring-primary-500 h-4 w-4 rounded",
            "dark:border-dark-secondary-400 dark:bg-dark-surface-50 dark:checked:bg-dark-primary-500 dark:focus:ring-dark-primary-500",
            className,
          )}
          {...props}
        />
        {label && (
          <span className="text-secondary-800 dark:text-dark-secondary-300 text-sm">
            {label}
          </span>
        )}
      </Label>
    );
  },
);
Checkbox.displayName = "Checkbox";
