import { forwardRef } from "react";
import { cn } from "../utils";

type SelectProps = React.SelectHTMLAttributes<HTMLSelectElement> & {
  options: Array<{ value: string; label: string }>;
  placeholder?: string;
};

export const Select = forwardRef<HTMLSelectElement, SelectProps>(
  ({ className, options, placeholder, ...props }, ref) => (
    <select
      ref={ref}
      className={cn(
        "w-full rounded-lg border px-3 py-2",
        "text-text dark:text-dark-text",
        "bg-surface-50 dark:bg-dark-surface-50",
        "border-surface-300 dark:border-dark-surface-600",
        "focus:ring-2 focus:outline-none",
        "focus:ring-primary-500 dark:focus:ring-dark-primary-500",
        "focus:border-transparent",
        "focus:ring-offset-2 focus:ring-offset-surface-50 dark:focus:ring-offset-dark-surface-100",
        className,
      )}
      {...props}
    >
      {placeholder && (
        <SelectOption value="" disabled hidden>
          {placeholder}
        </SelectOption>
      )}
      {options.map((option) => (
        <SelectOption key={option.value} value={option.value}>
          {option.label}
        </SelectOption>
      ))}
    </select>
  ),
);
Select.displayName = "Select";

type SelectOptionProps = React.OptionHTMLAttributes<HTMLOptionElement>;

export const SelectOption = forwardRef<HTMLOptionElement, SelectOptionProps>(
  ({ className, ...props }, ref) => (
    <option
      ref={ref}
      className={cn(
        "bg-surface-50 text-text dark:bg-dark-surface-50 dark:text-dark-text",
        className,
      )}
      {...props}
    />
  ),
);
SelectOption.displayName = "SelectOption";
