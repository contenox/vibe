import { forwardRef } from "react";
import { cn } from "../utils";
import { Spinner } from "./Spinner";
import { Span } from "./Typography";

type ButtonProps = React.ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "primary" | "secondary" | "ghost" | "accent" | "outline" | "text" | "danger" | "success";
  size?: "xs" | "sm" | "md" | "lg" | "xl" | "2xl" | "icon";
  isLoading?: boolean;
  palette?: "primary" | "secondary" | "accent" | "neutral" | "light";
  textAlign?: "center" | "bottom";
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
  (
    {
      className,
      variant = "primary",
      size = "md",
      palette = "primary",
      isLoading = false,
      textAlign = "center",
      ...props
    },
    ref,
  ) => {
    const paletteStyles = {
      primary: cn(
        "text-white dark:text-dark-text",
        variant !== "text" && "bg-primary dark:bg-dark-primary",
        "hover:bg-primary-600 dark:hover:bg-dark-primary-600",
        "focus:ring-primary-300 dark:focus:ring-dark-primary-300",
      ),
      secondary: cn(
        "text-on-secondary dark:text-dark-surface",
        variant !== "text" && "bg-secondary dark:bg-dark-secondary",
        "hover:bg-secondary-600 dark:hover:bg-dark-secondary-600",
        "focus:ring-secondary-300 dark:focus:ring-dark-secondary-300",
      ),
      accent: cn(
        "text-surface-inverted dark:text-dark-text",
        variant !== "text" && "bg-accent dark:bg-dark-accent",
        "hover:bg-accent-600 dark:hover:bg-dark-accent-600",
        "focus:ring-accent-300 dark:focus:ring-dark-accent-300",
      ),
      neutral: cn(
        "text-text dark:text-dark-text-muted",
        "hover:bg-surface-100 dark:hover:bg-dark-surface-100",
        "focus:ring-surface-300 dark:focus:ring-dark-surface-300",
      ),
      light: cn(
        "text-primary dark:text-dark-primary",
        "hover:bg-surface-50 dark:hover:bg-dark-surface-50",
        "focus:ring-primary-100 dark:focus:ring-dark-primary-100",
      ),
    };

    const ghostStyles = cn(
      "bg-transparent",
      "hover:bg-surface-100 dark:hover:bg-dark-surface-100",
      "text-current",
    );

    const dangerStyles = cn(
      "bg-transparent",
      "text-error dark:text-dark-error",
      "hover:bg-error/10 dark:hover:bg-dark-error/10",
      "focus:ring-error-300 dark:focus:ring-dark-error-300",
    );

    const successStyles = cn(
      "bg-transparent",
      "text-success dark:text-dark-success",
      "hover:bg-success/10 dark:hover:bg-dark-success/10",
      "focus:ring-success-300 dark:focus:ring-dark-success-300",
    );

    return (
      <button
        ref={ref}
        className={cn(
          "inline-flex flex-row items-center justify-center",
          "ease-fluid rounded-lg transition-all focus:ring-2 focus:ring-offset-2 focus:outline-none",
          "disabled:cursor-not-allowed disabled:opacity-50",
          {
            "px-3 py-1 text-xs": size === "xs",
            "px-4 py-1.5 text-sm": size === "sm",
            "px-6 py-2.5 text-base": size === "md",
            "px-8 py-3 text-lg": size === "lg",
            "px-10 py-4 text-xl": size === "xl",
            "px-12 py-5 text-2xl": size === "2xl",
            "p-2.5": size === "icon",
          },
          variant === "outline" &&
            cn(
              "border-2 bg-transparent",
              palette === "primary" &&
                "border-primary text-primary dark:border-dark-primary dark:text-dark-primary",
              palette === "secondary" &&
                "border-secondary text-secondary dark:border-dark-secondary dark:text-dark-secondary",
              palette === "accent" &&
                "border-accent text-accent dark:border-dark-accent dark:text-dark-accent",
              palette === "neutral" &&
                "border-surface-200 text-text dark:border-dark-surface-200 dark:text-dark-text",
            ),
          variant === "text" && "bg-transparent hover:bg-opacity-10",
          variant === "ghost"
            ? ghostStyles
            : variant === "danger"
              ? dangerStyles
              : variant === "success"
                ? successStyles
                : variant !== "outline" &&
                    variant !== "text" &&
                    paletteStyles[palette],
          className,
        )}
        disabled={isLoading || props.disabled}
        {...props}
      >
        {isLoading ? (
          <Span className="flex items-center gap-2">
            <Spinner
              size={
                size === "icon" || size === "xs"
                  ? "sm"
                  : size === "xl" || size === "2xl"
                    ? "lg"
                    : (size as "sm" | "md" | "lg")
              }
              className="mr-1"
            />
            <Span className={cn(textAlign === "bottom" && "self-end")}>
              {props.children}
            </Span>
          </Span>
        ) : (
          props.children
        )}
      </button>
    );
  },
);
Button.displayName = "Button";
