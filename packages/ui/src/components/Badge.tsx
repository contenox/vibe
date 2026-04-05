import { cn } from "../utils";

type BadgeVariant =
  | "default"
  | "primary"
  | "accent"
  | "success"
  | "error"
  | "warning"
  | "outline"
  | "secondary";

type BadgeSize = "sm" | "md";

type BadgeProps = React.HTMLAttributes<HTMLSpanElement> & {
  variant?: BadgeVariant;
  size?: BadgeSize;
};

export function Badge({
  className,
  variant = "default",
  size = "md",
  ...props
}: BadgeProps) {
  const baseStyles = cn(
    "inline-flex items-center font-medium rounded-full transition-colors",
    {
      "px-2.5 py-0.5 text-xs": size === "sm",
      "px-3 py-1 text-sm": size === "md",
    },
  );

  const variantStyles: Record<BadgeVariant, string> = {
    default: cn(
      "bg-[var(--color-primary-100)] text-[var(--color-primary-800)]",
      "dark:bg-[var(--color-dark-primary-900)] dark:text-[var(--color-dark-primary-300)]",
    ),
    primary: cn(
      "bg-[var(--color-primary-100)] text-[var(--color-primary-800)]",
      "dark:bg-[var(--color-dark-primary-900)] dark:text-[var(--color-dark-primary-300)]",
    ),
    accent: cn(
      "bg-[var(--color-accent-100)] text-[var(--color-accent-800)]",
      "dark:bg-[var(--color-dark-accent-900)] dark:text-[var(--color-dark-accent-300)]",
    ),
    success: cn(
      "bg-[var(--color-success-100)] text-[var(--color-success-800)]",
      "dark:bg-[var(--color-dark-success-900)] dark:text-[var(--color-dark-success-300)]",
    ),
    error: cn(
      "bg-[var(--color-error-100)] text-[var(--color-error-800)]",
      "dark:bg-[var(--color-dark-error-900)] dark:text-[var(--color-dark-error-300)]",
    ),
    warning: cn(
      "bg-[var(--color-warning-100)] text-[var(--color-warning-800)]",
      "dark:bg-[var(--color-dark-warning-900)] dark:text-[var(--color-dark-warning-300)]",
    ),
    secondary: cn(
      "bg-[var(--color-secondary-100)] text-[var(--color-secondary-800)]",
      "dark:bg-[var(--color-dark-secondary-900)] dark:text-[var(--color-dark-secondary-300)]",
    ),
    outline: cn(
      "bg-transparent border border-[var(--color-secondary-300)] text-[var(--color-secondary-700)]",
      "dark:border-[var(--color-dark-secondary-300)] dark:text-[var(--color-dark-secondary-300)]",
    ),
  };

  return (
    <span
      className={cn(baseStyles, variantStyles[variant], className)}
      {...props}
    />
  );
}
