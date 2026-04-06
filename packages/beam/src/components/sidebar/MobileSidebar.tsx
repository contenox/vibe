import { SidebarProps } from './Sidebar';
import { SidebarNav } from './SidebarNav';

// MobileSidebar.tsx
export function MobileSidebar({ isOpen, setIsOpen, items = [], children }: SidebarProps) {
  if (!isOpen) return null;

  return (
    <div className="fixed inset-x-0 top-20 bottom-0 z-50 overflow-x-hidden sm:hidden">
      <div
        className="bg-surface-100 dark:bg-dark-surface-100 fixed inset-x-0 top-20 bottom-0 z-40 min-h-0"
        onClick={() => setIsOpen(false)}
      />
      <div className="border-surface-300 dark:border-dark-surface-300 bg-surface dark:bg-dark-surface relative z-50 flex h-full min-h-0 flex-col border-r shadow-lg">
        {children ?? <SidebarNav items={items} setIsOpen={setIsOpen} />}
      </div>
    </div>
  );
}
