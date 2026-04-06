import React from "react";
import { cn } from "../utils";

type PageProps = {
  header?: React.ReactNode;
  footer?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  bodyScroll?: "auto" | "hidden";
};

export function Page({
  header,
  footer,
  children,
  className,
  bodyScroll = "auto",
}: PageProps) {
  return (
    <div className={cn("flex h-full min-h-0 flex-col", className)}>
      {header && <div className="shrink-0">{header}</div>}

      <div
        className={cn(
          "flex min-h-0 w-full max-w-full min-w-0 flex-1 flex-col overflow-x-clip",
          bodyScroll === "auto" ? "overflow-y-auto" : "overflow-y-hidden",
        )}
      >
        {children}
      </div>

      {footer && <div className="shrink-0">{footer}</div>}
    </div>
  );
}

export function Fill({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("relative min-h-0 min-w-0 flex-1", className)}>
      {children}
    </div>
  );
}
