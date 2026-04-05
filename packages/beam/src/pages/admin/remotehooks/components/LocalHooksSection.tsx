import { GridLayout, H2, P, Panel, Section } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { useLocalHooks } from '../../../../hooks/useRemoteHooks';
import { LocalHook } from '../../../../lib/types';
import LocalHookCard from './LocalHookCard';

export default function LocalHooksSection() {
  const { t } = useTranslation();
  const { data: localHooks, isLoading, error } = useLocalHooks();

  if (isLoading) {
    return (
      <Section className="flex justify-center py-8">
        <P variant="muted">{t('local_hooks.list_loading')}</P>
      </Section>
    );
  }

  if (error) {
    return <Panel variant="error">{t('local_hooks.list_error')}</Panel>;
  }

  return (
    <GridLayout variant="body">
      <Section>
        <H2 variant="sectionTitle" className="mb-4">
          {t('local_hooks.manage_title')}
        </H2>
        <P variant="muted" className="mb-6">
          {t('local_hooks.description')}
        </P>

        {localHooks && localHooks.length > 0 ? (
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
            {localHooks.map((hook: LocalHook) => (
              <LocalHookCard key={hook.name} hook={hook} />
            ))}
          </div>
        ) : (
          <Panel variant="bordered" className="py-12 text-center">
            <P variant="muted">{t('local_hooks.list_empty')}</P>
          </Panel>
        )}
      </Section>
    </GridLayout>
  );
}
