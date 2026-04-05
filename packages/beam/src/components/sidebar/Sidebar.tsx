import { useEffect } from 'react';
import { DesktopSidebar } from './DesktopSidebar';
import { MobileSidebar } from './MobileSidebar';

export type MenuItem = {
  path: string;
  label: string;
  icon?: React.ReactNode;
};

export type SidebarProps = {
  disabled: boolean;
  isOpen: boolean;
  setIsOpen: (open: boolean) => void;
  items: MenuItem[];
  className?: string;
};

export function Sidebar({ disabled, isOpen, setIsOpen, items, className }: SidebarProps) {
  useEffect(() => {
    if (disabled && isOpen) {
      setIsOpen(false);
    }
  }, [disabled, isOpen, setIsOpen]);

  if (disabled) {
    return <div />;
  }

  return (
    <div className="min-h-0 overflow-y-auto">
      <DesktopSidebar
        isOpen={isOpen}
        setIsOpen={setIsOpen}
        items={items}
        className={className}
        disabled={disabled}
      />
      <MobileSidebar isOpen={isOpen} setIsOpen={setIsOpen} items={items} disabled={disabled} />
    </div>
  );
}
