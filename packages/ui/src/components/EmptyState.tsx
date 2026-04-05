import { cn } from "../utils";
import { Section } from "./Section";
import { H3, P } from "./Typography";

type EmptyStateVariant = "default" | "error" | "warning" | "success" | "info";

type EmptyStateProps = {
  title: string;
  subtitle?: string;
  description: string;
  icon?: React.ReactNode;
  className?: string;
  orientation?: "vertical" | "horizontal";
  iconSize?: "sm" | "md" | "lg";
  variant?: EmptyStateVariant;
};

export function EmptyState({
  title,
  subtitle,
  description,
  icon,
  className,
  orientation = "vertical",
  iconSize = "md",
  variant = "default",
}: EmptyStateProps) {
  const variantStyles: Record<EmptyStateVariant, string> = {
    default: cn("text-text dark:text-dark-text"),
    info: cn(
      "text-text dark:text-dark-text",
      "bg-surface-50 dark:bg-dark-surface-50",
    ),
    success: cn(
      "text-[var(--color-success-800)] dark:text-[var(--color-dark-success-200)]",
      "bg-[var(--color-success-50)] dark:bg-[var(--color-dark-success-900)]",
    ),
    warning: cn(
      "text-[var(--color-warning-800)] dark:text-[var(--color-dark-warning-200)]",
      "bg-[var(--color-warning-50)] dark:bg-[var(--color-dark-warning-900)]",
    ),
    error: cn(
      "text-[var(--color-error-800)] dark:text-[var(--color-dark-error-200)]",
      "bg-[var(--color-error-50)] dark:bg-[var(--color-dark-error-900)]",
    ),
  };

  return (
    <Section
      title={title}
      className={cn(
        "p-8 rounded-xl",
        orientation === "horizontal"
          ? "flex items-center gap-6 text-left"
          : "text-center",
        variantStyles[variant],
        className,
      )}
    >
      {icon && (
        <div
          className={cn(
            "text-primary dark:text-dark-primary",
            orientation === "horizontal" ? "flex-shrink-0" : "mx-auto",
            {
              "text-3xl": iconSize === "lg",
              "text-2xl": iconSize === "md",
              "text-xl": iconSize === "sm",
            },
          )}
        >
          {icon}
        </div>
      )}
      {subtitle && (
        <P
          variant="lead"
          className="mb-4 text-text-muted dark:text-dark-text-muted"
        >
          {subtitle}
        </P>
      )}
      <P variant={orientation === "horizontal" ? undefined : "cardSubtitle"}>
        {description}
      </P>
    </Section>
  );
}
