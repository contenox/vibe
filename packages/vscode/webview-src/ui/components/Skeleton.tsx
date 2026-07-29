import { cn } from "../utils";

type SkeletonProps = React.HTMLAttributes<HTMLDivElement> & {
  variant?: "line" | "circle";
};

export function Skeleton({
  className,
  variant = "line",
  ...props
}: SkeletonProps) {
  return (
    <div
      className={cn(
        "bg-secondary-100 dark:bg-dark-surface-200 animate-pulse rounded-md",
        variant === "line" ? "h-4 w-full" : "h-8 w-8 rounded-full",
        className,
      )}
      {...props}
    />
  );
}
