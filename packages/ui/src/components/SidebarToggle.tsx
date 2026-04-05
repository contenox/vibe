import { Menu, X } from "lucide-react";
import { Button } from "./Button";

type SidebarToggleProps = {
  isOpen: boolean;
  onToggle: () => void;
};

export function SidebarToggle({ isOpen, onToggle }: SidebarToggleProps) {
  return (
    <Button
      variant="ghost"
      size="icon"
      onClick={onToggle}
      aria-label="Toggle sidebar"
    >
      {isOpen ? <X className="size-6" /> : <Menu className="size-6" />}
    </Button>
  );
}
