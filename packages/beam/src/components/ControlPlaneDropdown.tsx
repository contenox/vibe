import { Button } from '@contenox/ui';
import { t } from 'i18next';
import { LayoutGrid } from 'lucide-react';
import { adminNavItems } from '../config/routes';
import { cn } from '../lib/utils';
import { DropdownMenu } from './DropdownMenu';

export function ControlPlaneDropdown({
  isOpen,
  setIsOpen,
}: {
  isOpen: boolean;
  setIsOpen: (open: boolean) => void;
}) {
  const items = [
    {
      path: '/control',
      label: t('control_plane.all_tools'),
      icon: <LayoutGrid className="h-[1em] w-[1em]" aria-hidden />,
    },
    ...adminNavItems,
  ];

  return (
    <DropdownMenu
      isOpen={isOpen}
      setIsOpen={setIsOpen}
      items={items}
      contentClassName="min-w-[220px]"
      trigger={
        <Button
          variant="ghost"
          size="sm"
          aria-label={t('control_plane.menu_aria')}
          aria-haspopup="true"
          aria-expanded={isOpen}
          className="gap-1 px-2">
          <LayoutGrid className={cn('h-5 w-5', isOpen && 'text-primary-500 dark:text-dark-primary-500')} />
        </Button>
      }
    />
  );
}
