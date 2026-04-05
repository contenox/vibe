import { H1 } from "./Typography";
import { cn } from "../utils";
import React from "react";

interface ContainerProps extends React.HTMLAttributes<HTMLDivElement> {
  title?: string;
  padding?: string;
  innerPadding?: string;
}

export function Container({
  title,
  className,
  children,
  padding = "p-6",
  innerPadding = "p-4",
  ...rest
}: ContainerProps) {
  return (
    <div
      className={cn(`container mx-auto space-y-6`, padding, className)}
      {...rest}
    >
      {title && <H1>{title}</H1>}
      <div className={cn("bg-inherit", innerPadding)}>{children}</div>
    </div>
  );
}
