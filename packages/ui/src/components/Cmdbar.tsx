// components/Cmdbar.tsx
import { cn } from "../utils";
import { Button } from "./Button";
import { Panel } from "./Panel";
import { Span } from "./Typography";

type Item = {
  onClick: () => void;
  label: string;
  icon?: React.ReactNode;
};

interface CmdbarProps {
  items: Item[];
  className?: string;
}

export default function Cmdbar({ items, className }: CmdbarProps) {
  return (
    <Panel variant="bordered" className={cn("sticky top-0 z-50", className)}>
      <div className="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8">
        <div className="flex h-16 items-center justify-between">
          <div className="flex items-center gap-8">
            {items.map((item, index) => (
              <Button
                key={index}
                variant="ghost"
                className="rounded-lg px-3 py-2"
                onClick={item.onClick}
              >
                {item.icon && <Span className="mr-2" children={item.icon} />}
                {item.label}
              </Button>
            ))}
          </div>
        </div>
      </div>
    </Panel>
  );
}
