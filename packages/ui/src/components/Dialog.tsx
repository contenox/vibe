// components/Dialog.tsx
import { X } from "lucide-react";
import { cn } from "../utils";
import { Card } from "./Card";
import { H3 } from "./Typography";
import { Button } from "./Button";

type DialogProps = {
  open: boolean;
  onClose: () => void;
  title: string;
  children: React.ReactNode;
  className?: string;
};

export function Dialog({
  open,
  onClose,
  title,
  children,
  className,
}: DialogProps) {
  if (!open) return null;

  return (
    <div className="fixed inset-0 z-50 bg-black/50 backdrop-blur-sm" onClick={onClose}>
      <div className="fixed top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2" onClick={(e) => e.stopPropagation()}>
        <Card className={cn("w-[400px]", className)}>
          <div className="mb-4 flex items-center justify-between">
            <H3 className="text-primary-600 dark:text-dark-primary-500 text-lg font-semibold">
              {title}
            </H3>
            <Button
              onClick={onClose}
              className="text-secondary-500 hover:bg-secondary-100 dark:text-dark-secondary-400 dark:hover:bg-dark-surface-200 rounded-sm p-1"
            >
              <X className="h-5 w-5 dark:text-dark-secondary-400" />
            </Button>
          </div>
          {children}
        </Card>
      </div>
    </div>
  );
}
