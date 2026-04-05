import { Span } from '@contenox/ui';
import { Link } from 'react-router-dom';
import { cn } from '../../lib/utils';
import { MenuItem } from './Sidebar';

export function SidebarNav({
  items,
  setIsOpen,
}: {
  items: MenuItem[];
  setIsOpen: (open: boolean) => void;
}) {
  return (
    <nav className="flex-1 space-y-2 p-4">
      {items.map(item => (
        <Link
          key={item.path}
          to={item.path}
          onClick={() => setIsOpen(false)}
          className={cn(
            'flex items-center gap-3 rounded-lg px-4 py-2.5',
            'text-text dark:text-dark-text hover:bg-surface-100 dark:hover:bg-dark-surface-100 transition-colors',
          )}>
          {item.icon && <Span className="text-primary dark:text-dark-primary">{item.icon}</Span>}
          <Span>{item.label}</Span>
        </Link>
      ))}
    </nav>
  );
}
