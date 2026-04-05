// components/SearchBar.tsx
import { Search, X } from "lucide-react";
import { forwardRef } from "react";
import { cn } from "../utils";
import { Button } from "./Button";
import { Inbox } from "./Inbox";

type SearchBarProps = React.InputHTMLAttributes<HTMLInputElement> & {
  onClear?: () => void;
};

export const SearchBar = forwardRef<HTMLInputElement, SearchBarProps>(
  ({ className, value, onClear, ...props }, ref) => {
    return (
      <div className="relative w-full">
        <Search className="text-secondary-400 dark:text-dark-secondary-400 absolute top-1/2 left-3 h-5 w-5 -translate-y-1/2" />
        <Inbox
          ref={ref}
          value={value}
          className={cn(
            "border-secondary-300 bg-surface-50 w-full rounded-lg border py-2.5 pr-8 pl-10",
            "focus:ring-primary-500 focus:ring-2 focus:ring-offset-2",
            "dark:border-dark-secondary-300 dark:bg-dark-surface-50 dark:text-dark-text",
            className,
          )}
          {...props}
        />
        {value && (
          <Button
            onClick={onClear}
            className="absolute top-1/2 right-3 -translate-y-1/2 text-secondary-400 hover:text-secondary-600 dark:text-dark-secondary-400 dark:hover:text-dark-secondary-600"
          >
            <X className="h-5 w-5" />
          </Button>
        )}
      </div>
    );
  },
);
SearchBar.displayName = "SearchBar";
