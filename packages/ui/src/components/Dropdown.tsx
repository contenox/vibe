import React, { useState, useEffect, useRef, useCallback } from "react";
import { ChevronDown } from "lucide-react";
import { cn } from "../utils";
import { Button } from "./Button";

export interface DropdownProps {
  isOpen?: boolean;
  onToggle?: (isOpen: boolean) => void;
  trigger?: React.ReactElement<{ onClick?: React.MouseEventHandler<Element> }>;
  options?: { value: string; label: string }[];
  value?: string;
  onChange?: (value: string) => void;
  children?: React.ReactNode;
  contentClassName?: string;
  className?: string;
}

export function Dropdown({
  isOpen: controlledOpen,
  onToggle,
  trigger,
  options,
  value,
  onChange,
  children,
  contentClassName,
  className,
}: DropdownProps) {
  const [internalOpen, setInternalOpen] = useState(false);
  const dropdownRef = useRef<HTMLDivElement>(null);
  const isControlled = controlledOpen !== undefined;
  const isOpen = isControlled ? controlledOpen : internalOpen;

  const toggle = () => {
    if (!isControlled) setInternalOpen(!isOpen);
    onToggle?.(!isOpen);
  };

  const close = () => {
    if (!isControlled) setInternalOpen(false);
    onToggle?.(false);
  };

  const closeRef = useRef(close);
  closeRef.current = close;

  useEffect(() => {
    const handleClickOutside = (event: MouseEvent) => {
      if (
        dropdownRef.current &&
        !dropdownRef.current.contains(event.target as Node)
      ) {
        closeRef.current();
      }
    };

    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, []);

  const triggerElement = trigger ? (
    React.cloneElement(trigger, {
      onClick: (e: React.MouseEvent) => {
        e.stopPropagation();
        trigger.props.onClick?.(e);
        toggle();
      },
      "aria-haspopup": true,
      "aria-expanded": isOpen,
    } as React.HTMLAttributes<HTMLElement>)
  ) : options ? (
    <Button
      onClick={toggle}
      aria-haspopup="true"
      aria-expanded={isOpen}
      className={cn(
        "border-secondary-300 bg-surface-50 flex w-full items-center justify-between rounded-lg border px-4 py-2.5",
        "focus:ring-primary-500 focus:ring-2 focus:ring-offset-2",
        "dark:border-dark-secondary-300 dark:bg-dark-surface-50",
      )}
    >
      <span className="text-text dark:text-dark-text">
        {options.find((opt) => opt.value === value)?.label || "Select"}
      </span>
      <ChevronDown className="text-secondary-400 dark:text-dark-secondary-400 h-5 w-5" />
    </Button>
  ) : null;

  const content = children
    ? children
    : options
      ? options.map((option) => (
          <Button
            key={option.value}
            role="menuitem"
            onClick={() => {
              onChange?.(option.value);
              close();
            }}
            className={cn(
              "text-text hover:bg-secondary-100 w-full px-4 py-2 text-left",
              "dark:text-dark-text dark:hover:bg-dark-surface-100",
              option.value === value &&
                "bg-primary-50 dark:bg-dark-primary-900",
            )}
          >
            {option.label}
          </Button>
        ))
      : null;

  return (
    <div className={cn("relative", className)} ref={dropdownRef}>
      {triggerElement}
      {isOpen && (
        <div
          className={cn(
            "absolute z-50 mt-2 w-full rounded-lg border shadow-lg",
            contentClassName,
          )}
          role="menu"
          aria-hidden={!isOpen}
        >
          {content}
        </div>
      )}
    </div>
  );
}
