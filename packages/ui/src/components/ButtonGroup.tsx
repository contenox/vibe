import { cn } from "../utils";

export function ButtonGroup({
  children,
  className,
}: React.HTMLAttributes<HTMLDivElement>) {
  return <div className={cn("flex gap-2 shrink-0", className)}>{children}</div>;
}
