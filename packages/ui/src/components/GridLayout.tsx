import { cn } from "../utils";
import React from "react";
import { Section } from "./Section";

type GridLayoutVariant = "surface" | "bordered" | "body";

interface GridLayoutProps extends React.HTMLAttributes<HTMLDivElement> {
  title?: string;
  description?: string;
  minWidth?: string;
  columns?: number;
  responsive?: {
    base?: number;
    sm?: number;
    md?: number;
    lg?: number;
    xl?: number;
  };
  variant?: GridLayoutVariant;
}

export function GridLayout({
  title,
  description,
  minWidth = "minmax(400px, 1fr)",
  columns = 0,
  responsive,
  variant = "bordered",
  className,
  children,
  ...props
}: GridLayoutProps) {
  let inlineStyle: React.CSSProperties | undefined = undefined;
  let responsiveClasses = "";

  if (responsive) {
    const breakpoints: { [key: string]: string } = {
      base: "",
      sm: "sm:",
      md: "md:",
      lg: "lg:",
      xl: "xl:",
    };

    const entries = Object.entries({
      base: responsive.base ?? 1,
      ...("sm" in responsive ? { sm: responsive.sm } : {}),
      ...("md" in responsive ? { md: responsive.md } : {}),
      ...("lg" in responsive ? { lg: responsive.lg } : {}),
      ...("xl" in responsive ? { xl: responsive.xl } : {}),
    });

    responsiveClasses = entries
      .map(([bp, value]) => `${breakpoints[bp]}grid-cols-${value}`)
      .join(" ");
  } else {
    inlineStyle = {
      gridTemplateColumns: columns
        ? `repeat(${columns}, 1fr)`
        : `repeat(auto-fit, ${minWidth})`,
    };
  }

  return (
    <Section
      title={title}
      description={description}
      variant={variant}
      {...props}
    >
      <div
        className={cn(
          "grid gap-4 min-w-0 overflow-x-hidden",
          "[&>*]:min-w-0 [&>*]:m-0",
          responsiveClasses,
          className,
        )}
        style={inlineStyle}
      >
        {children}
      </div>
    </Section>
  );
}
