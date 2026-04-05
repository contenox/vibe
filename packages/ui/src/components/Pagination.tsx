// components/Pagination.tsx
import { ChevronLeft, ChevronRight } from "lucide-react";
import { cn } from "../utils";
import { Button } from "./Button";
import { Span } from "./Typography";

type PaginationProps = {
  currentPage: number;
  totalPages: number;
  onPageChange: (page: number) => void;
  className?: string;
};

export function Pagination({
  currentPage,
  totalPages,
  onPageChange,
  className,
}: PaginationProps) {
  return (
    <div
      className={cn("flex items-center justify-between px-4 py-3", className)}
    >
      <Button
        onClick={() => onPageChange(Math.max(1, currentPage - 1))}
        disabled={currentPage === 1}
        className={cn(
          "flex items-center gap-1 rounded-lg px-3 py-1.5",
          "text-secondary-600 hover:bg-secondary-100",
          "dark:text-dark-secondary-400 dark:hover:bg-dark-surface-200",
          "disabled:opacity-50 disabled:hover:bg-transparent",
        )}
      >
        <ChevronLeft className="h-4 w-4 dark:text-dark-secondary-400" />
        Previous
      </Button>

      <Span className="text-secondary-600 dark:text-dark-secondary-400 text-sm">
        Page {currentPage} of {totalPages}
      </Span>

      <Button
        onClick={() => onPageChange(Math.min(totalPages, currentPage + 1))}
        disabled={currentPage === totalPages}
        className={cn(
          "flex items-center gap-1 rounded-lg px-3 py-1.5",
          "text-secondary-600 hover:bg-secondary-100",
          "dark:text-dark-secondary-400 dark:hover:bg-dark-surface-200",
          "disabled:opacity-50 disabled:hover:bg-transparent",
        )}
      >
        Next
        <ChevronRight className="h-4 w-4 dark:text-dark-secondary-400" />
      </Button>
    </div>
  );
}
