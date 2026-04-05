import { ArrowDown } from "lucide-react";
import { cn } from "../../utils";
import { Button } from "../Button";

export type ChatScrollToLatestProps = {
  visible: boolean;
  onClick: () => void;
  label?: string;
  className?: string;
};

export function ChatScrollToLatest({
  visible,
  onClick,
  label,
  className,
}: ChatScrollToLatestProps) {
  if (!visible) return null;

  return (
    <div
      className={cn(
        "pointer-events-none absolute inset-x-0 bottom-4 flex justify-center",
        className,
      )}
    >
      <Button
        variant="secondary"
        size="sm"
        onClick={onClick}
        className="pointer-events-auto shadow-lg"
        aria-label={label}
      >
        <ArrowDown className="mr-1.5 h-3.5 w-3.5" />
        {label}
      </Button>
    </div>
  );
}
