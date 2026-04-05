// components/Accordion.tsx
import { ChevronDown } from "lucide-react";
import { useState } from "react";
import { cn } from "../utils";
import { Button } from "./Button";
import { Span } from "./Typography";

type AccordionProps = {
  title: string;
  children: React.ReactNode;
  className?: string;
};

export function Accordion({ title, children, className }: AccordionProps) {
  const [isOpen, setIsOpen] = useState(false);

  return (
    <div
      className={cn(
        "border-secondary-200 dark:border-dark-secondary-300 rounded-lg border",
        className,
      )}
    >
      <Button
        onClick={() => setIsOpen(!isOpen)}
        className="flex w-full items-center justify-between p-4"
      >
        <Span className="text-secondary-800 dark:text-dark-secondary-200 text-sm font-medium">
          {title}
        </Span>
        <ChevronDown
          className={cn(
            "text-secondary-400 dark:text-dark-secondary-400 h-5 w-5 transition-transform",
            isOpen && "rotate-180",
          )}
        />
      </Button>
      <div
        className={cn(
          "overflow-hidden transition-all",
          isOpen ? "max-h-[1000px] opacity-100" : "max-h-0 opacity-0",
        )}
      >
        <div className="p-4 pt-0">{children}</div>
      </div>
    </div>
  );
}
