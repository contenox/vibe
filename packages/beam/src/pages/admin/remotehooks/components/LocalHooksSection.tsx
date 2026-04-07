import { EmptyState, GridLayout, LoadingState, Panel, Section } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { useLocalHooks } from '../../../../hooks/useRemoteHooks';
import { LocalHook } from '../../../../lib/types';
import LocalHookCard from './LocalHookCard';

export default function LocalHooksSection() {
  const { t } = useTranslation();
  const { data: localHooks, isLoading, error } = useLocalHooks();

  if (isLoading) {
    return <LoadingState message={t('local_hooks.list_loading')} />;
  }

  if (error) {
    return <Panel variant="error">{t('local_hooks.list_error')}</Panel>;
  }

  return (
    <GridLayout variant="body">
      <Section title={t('local_hooks.manage_title')} description={t('local_hooks.description')}>
        {localHooks && localHooks.length > 0 ? (
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
            {localHooks.map((hook: LocalHook) => (
              <LocalHookCard key={hook.name} hook={hook} />
            ))}
          </div>
        ) : (
          <EmptyState title={t('local_hooks.list_empty')} />
        )}
      </Section>
    </GridLayout>
  );
}
