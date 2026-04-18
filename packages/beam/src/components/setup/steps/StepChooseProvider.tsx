import { H2, P } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { cn } from '../../../lib/utils';

export type ProviderChoice = 'local' | 'ollama' | 'openai' | 'gemini' | 'vertex';

type ProviderCardProps = {
  id: ProviderChoice;
  title: string;
  desc: string;
  selected: boolean;
  onSelect: (id: ProviderChoice) => void;
};

function ProviderCard({ id, title, desc, selected, onSelect }: ProviderCardProps) {
  return (
    <button
      type="button"
      onClick={() => onSelect(id)}
      className={cn(
        'w-full rounded-lg border p-4 text-left transition-colors',
        selected
          ? 'border-primary-500 bg-primary-50 dark:bg-primary-950/30'
          : 'border-surface-200 hover:border-surface-400 dark:border-dark-surface-600',
      )}>
      <div className="flex items-start gap-3">
        <div
          className={cn(
            'mt-0.5 h-4 w-4 shrink-0 rounded-full border-2 transition-colors',
            selected
              ? 'border-primary-500 bg-primary-500'
              : 'border-surface-400 dark:border-dark-surface-400',
          )}
        />
        <div>
          <P className="font-semibold text-sm">{title}</P>
          <P variant="muted" className="text-xs mt-0.5">
            {desc}
          </P>
        </div>
      </div>
    </button>
  );
}

type Props = {
  value: ProviderChoice;
  onChange: (v: ProviderChoice) => void;
};

export default function StepChooseProvider({ value, onChange }: Props) {
  const { t } = useTranslation();

  const providers: { id: ProviderChoice; titleKey: string; descKey: string }[] = [
    { id: 'local', titleKey: 'onboarding.step_choose_provider.local_title', descKey: 'onboarding.step_choose_provider.local_desc' },
    { id: 'ollama', titleKey: 'onboarding.step_choose_provider.ollama_title', descKey: 'onboarding.step_choose_provider.ollama_desc' },
    { id: 'openai', titleKey: 'onboarding.step_choose_provider.openai_title', descKey: 'onboarding.step_choose_provider.openai_desc' },
    { id: 'gemini', titleKey: 'onboarding.step_choose_provider.gemini_title', descKey: 'onboarding.step_choose_provider.gemini_desc' },
    { id: 'vertex', titleKey: 'onboarding.step_choose_provider.vertex_title', descKey: 'onboarding.step_choose_provider.vertex_desc' },
  ];

  return (
    <div className="max-w-xl mx-auto space-y-6">
      <div className="space-y-1">
        <H2 className="text-xl font-semibold">{t('onboarding.step_choose_provider.title')}</H2>
        <P variant="muted" className="text-sm">
          {t('onboarding.step_choose_provider.desc')}
        </P>
      </div>
      <div className="space-y-3">
        {providers.map(p => (
          <ProviderCard
            key={p.id}
            id={p.id}
            title={t(p.titleKey as Parameters<typeof t>[0])}
            desc={t(p.descKey as Parameters<typeof t>[0])}
            selected={value === p.id}
            onSelect={onChange}
          />
        ))}
      </div>
    </div>
  );
}
