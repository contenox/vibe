import { ReactNode } from "react";
import { cn } from "../utils";

type TypographyVariant =
  | "hero"
  | "lead"
  | "page"
  | "cardTitle"
  | "cardSubtitle"
  | "footerTitle"
  | "footerText"
  | "sectionTitle"
  | "subsectionTitle"
  | "body"
  | "status"
  | "muted"
  | "caption";

type TypographyProps = {
  children: ReactNode;
  className?: string;
  variant?: TypographyVariant;
};

export function H1({ children, className, variant }: TypographyProps) {
  return (
    <h1
      className={cn(
        "text-text dark:text-dark-text",
        variant === "hero"
          ? "text-5xl md:text-6xl font-bold leading-tight"
          : variant === "sectionTitle"
            ? "text-4xl font-display font-bold"
            : variant === "page"
              ? "text-2xl font-semibold"
              : "text-2xl font-bold",
        className,
      )}
    >
      {children}
    </h1>
  );
}

export function H2({ children, className, variant }: TypographyProps) {
  return (
    <h2
      className={cn(
        "font-semibold text-text dark:text-dark-text",
        variant === "footerTitle"
          ? "text-lg"
          : variant === "sectionTitle"
            ? "text-3xl font-display font-bold"
            : "text-xl",
        className,
      )}
    >
      {children}
    </h2>
  );
}

export function H3({ children, className, variant }: TypographyProps) {
  return (
    <h3
      className={cn(
        "text-text dark:text-dark-text",
        variant === "subsectionTitle"
          ? "text-xl font-semibold"
          : "text-lg font-medium",
        className,
      )}
    >
      {children}
    </h3>
  );
}

export function P({ children, className, variant }: TypographyProps) {
  return (
    <p
      className={cn(
        "text-text dark:text-dark-text",
        variant === "lead"
          ? "text-xl leading-relaxed"
          : variant === "cardSubtitle"
            ? "text-sm text-text-muted dark:text-dark-text-muted"
            : variant === "footerText"
              ? "text-sm text-text-muted dark:text-dark-text-muted"
              : variant === "body"
                ? "text-base"
                : variant === "caption"
                  ? "text-xs text-text-muted uppercase tracking-wide"
                  : "text-base",
        variant === "status"
          ? "text-xs uppercase tracking-wider font-medium"
          : className,
      )}
    >
      {children}
    </p>
  );
}

export function Small({ children, className }: TypographyProps) {
  return (
    <p className={cn("text-sm text-text dark:text-dark-text-muted", className)}>
      {children}
    </p>
  );
}

export function Span({ children, className, variant }: TypographyProps) {
  return (
    <span
      className={cn(
        "text-[var(--color-text)] dark:text-[var(--color-dark-text)]",
        {
          "text-xs uppercase tracking-wider font-medium": variant === "status",
          "text-sm text-text-muted dark:text-dark-text-muted":
            variant === "muted",
        },
        className,
      )}
    >
      {children}
    </span>
  );
}

export function Blockquote({ children, className }: TypographyProps) {
  return (
    <blockquote
      className={cn(
        "border-l-4 pl-4 italic text-text dark:text-dark-text",
        className,
      )}
    >
      {children}
    </blockquote>
  );
}
