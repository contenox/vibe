import React, { createContext, useContext, useState } from "react";
import { cn } from "../utils";
import { Button } from "./Button";

interface CollapsibleContextType {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

const CollapsibleContext = createContext<CollapsibleContextType | undefined>(
  undefined,
);

interface CollapsibleProps {
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  defaultOpen?: boolean;
  defaultExpanded?: boolean;
  title?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}

export const Collapsible = ({
  open: controlledOpen,
  onOpenChange,
  defaultOpen,
  defaultExpanded,
  title,
  children,
  className,
}: CollapsibleProps) => {
  const [internalOpen, setInternalOpen] = useState<boolean>(() => {
    if (controlledOpen !== undefined) return controlledOpen;
    if (defaultOpen !== undefined) return defaultOpen;
    if (defaultExpanded !== undefined) return defaultExpanded;
    return false;
  });

  const isControlled = controlledOpen !== undefined;
  const open = isControlled ? controlledOpen : internalOpen;

  const handleOpenChange = (newOpen: boolean) => {
    if (!isControlled) {
      setInternalOpen(newOpen);
    }
    onOpenChange?.(newOpen);
  };

  return (
    <CollapsibleContext.Provider
      value={{ open, onOpenChange: handleOpenChange }}
    >
      <div className={cn("w-full", className)}>
        {title ? (
          <>
            {" "}
            <CollapsibleTrigger className="flex w-full items-center justify-between rounded-md bg-surface-50 dark:bg-dark-surface-50 border border-surface-300 dark:border-dark-surface-300 px-3 py-2 text-left text-text dark:text-dark-text hover:bg-surface-100 dark:hover:bg-dark-surface-100">
              {" "}
              <span>{title}</span>
              <span
                aria-hidden
                className="text-text-muted dark:text-dark-text-muted"
              >
                {open ? "−" : "+"}
              </span>
            </CollapsibleTrigger>
            <CollapsibleContent>{children}</CollapsibleContent>
          </>
        ) : (
          children
        )}
      </div>
    </CollapsibleContext.Provider>
  );
};

interface CollapsibleTriggerProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  asChild?: boolean;
  children: React.ReactNode;
  className?: string;
}

export const CollapsibleTrigger = ({
  asChild = false,
  children,
  className,
  ...props
}: CollapsibleTriggerProps) => {
  const context = useContext(CollapsibleContext);

  if (!context) {
    throw new Error("CollapsibleTrigger must be used within a Collapsible");
  }

  const { open, onOpenChange } = context;

  const handleClick = () => {
    onOpenChange(!open);
  };

  if (asChild && React.isValidElement(children)) {
    return React.cloneElement(children, {
      onClick: handleClick,
      "aria-expanded": open,
      "data-state": open ? "open" : "closed",
      ...props,
    } as React.HTMLAttributes<HTMLElement>);
  }

  return (
    <Button
      type="button"
      onClick={handleClick}
      aria-expanded={open}
      data-state={open ? "open" : "closed"}
      className={cn(
        "flex w-full items-center justify-between",
        "transition-colors duration-200",
        "focus:outline-none focus:ring-2",
        "focus:outline-none focus:ring-2",
        "focus:ring-primary-500 dark:focus:ring-dark-primary-500",
        "focus:ring-offset-2 focus:ring-offset-surface-50 dark:focus:ring-offset-dark-surface-50",
        className,
      )}
      {...props}
    >
      {children}
    </Button>
  );
};

interface CollapsibleContentProps {
  children: React.ReactNode;
  className?: string;
}

export const CollapsibleContent = ({
  children,
  className,
}: CollapsibleContentProps) => {
  const context = useContext(CollapsibleContext);

  if (!context) {
    throw new Error("CollapsibleContent must be used within a Collapsible");
  }

  const { open } = context;

  return (
    <div
      data-state={open ? "open" : "closed"}
      className={cn(
        "overflow-hidden transition-all duration-300 ease-in-out",
        open
          ? "animate-in fade-in-0 slide-in-from-top-2"
          : "animate-out fade-out-0 slide-out-to-top-2",
        !open && "hidden",
        className,
      )}
    >
      {children}
    </div>
  );
};
