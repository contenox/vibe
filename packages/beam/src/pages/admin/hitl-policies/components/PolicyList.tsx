import { Button } from '@contenox/ui';
import { useTranslation } from 'react-i18next';

interface Props {
  names: string[];
  activeName: string;
  selectedName: string;
  onSelect: (name: string) => void;
  onSetActive: (name: string) => void;
  onDelete: (name: string) => void;
}

export default function PolicyList({ names, activeName, selectedName, onSelect, onSetActive, onDelete }: Props) {
  const { t } = useTranslation();

  return (
    <div className="flex w-64 flex-col gap-1 overflow-y-auto border-r border-neutral-200 p-3 dark:border-neutral-700">
      {names.length === 0 && (
        <p className="text-sm text-neutral-500">{t('hitl_policies.list_empty')}</p>
      )}
      {names.map(name => (
        <div
          key={name}
          className={`group flex flex-col gap-1 rounded-md p-2 cursor-pointer hover:bg-neutral-100 dark:hover:bg-neutral-800 ${
            selectedName === name ? 'bg-neutral-100 dark:bg-neutral-800' : ''
          }`}
          onClick={() => onSelect(name)}>
          <div className="flex items-center justify-between gap-1">
            <span className="truncate text-sm font-medium">{name}</span>
            {activeName === name && (
              <span className="rounded bg-green-100 px-1.5 py-0.5 text-xs font-medium text-green-700 dark:bg-green-900 dark:text-green-300">
                {t('hitl_policies.active')}
              </span>
            )}
          </div>
          <div className="flex gap-1 opacity-0 group-hover:opacity-100">
            {activeName !== name && (
              <Button
                variant="secondary"
                size="sm"
                onClick={e => { e.stopPropagation(); onSetActive(name); }}>
                {t('hitl_policies.set_active')}
              </Button>
            )}
            <Button
              variant="secondary"
              size="sm"
              onClick={e => { e.stopPropagation(); void onDelete(name); }}>
              {t('common.delete')}
            </Button>
          </div>
        </div>
      ))}
    </div>
  );
}
