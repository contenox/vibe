import { Button, Input, P, Select } from '@contenox/ui';
import { useTranslation } from 'react-i18next';

interface ValidConditionsEditorProps {
  value?: Record<string, boolean>;
  onChange: (next: Record<string, boolean>) => void;
}

export default function ValidConditionsEditor({ value, onChange }: ValidConditionsEditorProps) {
  const { t } = useTranslation();
  const conditions = value || {};

  const updateCondition = (oldKey: string, newKey: string, newValue: boolean) => {
    const next: Record<string, boolean> = { ...conditions };
    if (oldKey !== newKey) {
      delete next[oldKey];
    }
    next[newKey] = newValue;
    onChange(next);
  };

  const removeCondition = (key: string) => {
    const next = { ...conditions };
    delete next[key];
    onChange(next);
  };

  const addCondition = () => {
    onChange({ ...conditions, '': true });
  };

  return (
    <div className="space-y-2">
      <label className="block text-sm font-medium">{t('chains.task_form.valid_conditions')}</label>

      {Object.entries(conditions).map(([key, val], index) => (
        <div key={index} className="flex items-center gap-2">
          <Input
            value={key}
            onChange={(e: React.ChangeEvent<HTMLInputElement>) =>
              updateCondition(key, e.target.value, val)
            }
            placeholder="condition_key"
            className="flex-1"
          />
          <Select
            value={val ? 'true' : 'false'}
            onChange={e => updateCondition(key, key, e.target.value === 'true')}
            options={[
              { value: 'true', label: t('chains.task_form.true') },
              { value: 'false', label: t('chains.task_form.false') },
            ]}
          />
          <Button
            type="button"
            onClick={() => removeCondition(key)}
            className="text-error hover:bg-error rounded-md px-3 py-2 transition-colors hover:text-white">
            ×
          </Button>
        </div>
      ))}

      <Button type="button" variant="secondary" size="sm" onClick={addCondition}>
        {t('chains.task_form.add_condition')}
      </Button>

      <P>{t('chains.task_form.valid_conditions_help')}</P>
    </div>
  );
}
