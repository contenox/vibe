import { cloneElement, isValidElement, useEffect } from 'react';
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
  /** When omitted and `children` is not set, the rail is empty. */
  items?: MenuItem[];
  /** Replaces the default nav list when provided. */
  children?: React.ReactNode;
  className?: string;
};

export function Sidebar({ disabled, isOpen, setIsOpen, items = [], children, className }: SidebarProps) {
  useEffect(() => {
    if (disabled && isOpen) {
      setIsOpen(false);
    }
  }, [disabled, isOpen, setIsOpen]);

  if (disabled) {
    return <div />;
  }

  const rail =
    children && isValidElement(children)
      ? cloneElement(children as React.ReactElement<{ setIsOpen?: (open: boolean) => void }>, {
          setIsOpen,
        })
      : children;

  return (
    <div className="flex h-full min-h-0 flex-col">
      <DesktopSidebar
        isOpen={isOpen}
        setIsOpen={setIsOpen}
        items={items}
        className={className}
        disabled={disabled}>
        {rail}
      </DesktopSidebar>
      <MobileSidebar isOpen={isOpen} setIsOpen={setIsOpen} items={items} disabled={disabled}>
        {rail}
      </MobileSidebar>
    </div>
  );
}
