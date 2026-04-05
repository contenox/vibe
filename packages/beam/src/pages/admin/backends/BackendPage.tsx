// src/pages/admin/backends/index.tsx
import { TabbedPage } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';
import { Page } from '../../../components/Page';
import { useRuntimeBackendState } from '../../../hooks/useRuntimeBackendState';
import BackendsSection from './components/BackendsSection';
import CloudProvidersSection from './components/CloudProvidersSection';
import RuntimeStateSection from './components/RuntimeStateSection';

const BACKEND_TAB_IDS = ['backends', 'cloud-providers', 'state'] as const;

export default function BackendsPage() {
  const { t } = useTranslation();
  const [searchParams, setSearchParams] = useSearchParams();
  const rawTab = searchParams.get('tab') ?? 'backends';
  const activeTab = (
    BACKEND_TAB_IDS as readonly string[]
  ).includes(rawTab)
    ? rawTab
    : 'backends';
  const runtime = useRuntimeBackendState();

  const tabs = [
    {
      id: 'backends',
      label: t('backends.manage_title'),
      content: <BackendsSection />,
    },
    {
      id: 'cloud-providers',
      label: t('cloud_providers.title'),
      content: <CloudProvidersSection />,
    },
    {
      id: 'state',
      label: t('state.runtime_tab'),
      content: (
        <RuntimeStateSection
          data={runtime.data}
          isLoading={runtime.isLoading}
          isError={runtime.isError}
          errorMessage={runtime.error?.message}
        />
      ),
    },
  ];

  return (
    <Page bodyScroll="auto" className="h-full">
      <TabbedPage
        tabs={tabs}
        className="h-full"
        activeTab={activeTab}
        onTabChange={id => {
          setSearchParams(
            prev => {
              const next = new URLSearchParams(prev);
              next.set('tab', id);
              return next;
            },
            { replace: true },
          );
        }}
      />
    </Page>
  );
}
