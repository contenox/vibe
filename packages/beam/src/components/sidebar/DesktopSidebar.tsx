// DesktopSidebar.tsx
import { cn } from '../../lib/utils';
import { SidebarProps } from './Sidebar';
import { SidebarNav } from './SidebarNav';

export function DesktopSidebar({ isOpen, setIsOpen, items, className }: SidebarProps) {
  return (
    <div
      className={cn(
        'hidden sm:flex sm:shrink-0 sm:flex-col sm:overflow-hidden',
        isOpen
          ? 'border-surface-300 dark:border-dark-surface-700 min-h-0 w-64 border-r shadow-lg'
          : 'w-0 border-0 shadow-none',
        'bg-surface dark:bg-dark-surface-100 overflow-x-hidden',
        className,
      )}>
      <div className="flex h-full w-full flex-col">
        <SidebarNav items={items} setIsOpen={setIsOpen} />
      </div>
    </div>
  );
}
