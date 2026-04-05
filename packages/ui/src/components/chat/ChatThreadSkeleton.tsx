import { cn } from "../../utils";
import { Skeleton } from "../Skeleton";

export type ChatThreadSkeletonProps = {
  rows?: number;
  className?: string;
};

export function ChatThreadSkeleton({
  rows = 5,
  className,
}: ChatThreadSkeletonProps) {
  return (
    <div className={cn("flex h-full flex-col gap-4 p-6", className)}>
      {Array.from({ length: rows }).map((_, index) => (
        <div key={index} className="flex gap-3">
          <Skeleton variant="circle" className="mt-1" />
          <div className="flex-1 space-y-2">
            <Skeleton variant="line" className="h-4 w-32" />
            <Skeleton variant="line" className="h-16 w-full" />
          </div>
        </div>
      ))}
    </div>
  );
}
