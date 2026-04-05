import { Button, Dropdown, Section, Span } from '@contenox/ui';
import { t } from 'i18next';
import { ChevronDown } from 'lucide-react';
import { Link } from 'react-router-dom';
import { cn } from '../lib/utils';

type MenuItem = {
  path: string;
  label: string;
  icon?: React.ReactNode;
};

type NavDropdownProps = {
  isOpen: boolean;
  setIsOpen: (open: boolean) => void;
  items: MenuItem[];
};

export function DropdownMenu({ isOpen, setIsOpen, items }: NavDropdownProps) {
  return (
    <Dropdown
      isOpen={isOpen}
      onToggle={setIsOpen}
      trigger={
        <Button variant="ghost" size="sm" aria-label={t('common.menu')} className="gap-1">
          <ChevronDown className={cn('h-8 w-4 transition-transform', isOpen && 'rotate-180')} />
        </Button>
      }
      contentClassName="absolute right-0 top-full mt-2 min-w-[160px]">
      <Section>
        <nav className="py-2">
          {items.map(item => (
            <Link
              key={item.path}
              to={item.path}
              className={cn(
                'flex items-center gap-2 px-4 py-2 text-sm',
                'hover:bg-primary-600 dark:hover:bg-dark-primary-600',
                'transition-colors duration-200',
              )}
              onClick={() => setIsOpen(false)}>
              {item.icon && (
                <Span className="text-primary-500 dark:text-dark-primary-500 h-4 w-4">
                  {item.icon}
                </Span>
              )}
              <Span variant="body">{item.label}</Span>
            </Link>
          ))}
        </nav>
      </Section>
    </Dropdown>
  );
}
