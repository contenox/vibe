import { Page, Panel, TabbedPage } from '@contenox/ui';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';
import LocalHooksSection from './components/LocalHooksSection';
import McpServersSection from './components/McpServersSection';
import RemoteHooksSection from './components/RemoteHooksSection';

type HooksTabId = 'mcp-servers' | 'remote-hooks' | 'discovery';

/** Default to MCP servers so the page stays usable when Discovery would enumerate many broken MCP configs. */
const DEFAULT_TAB: HooksTabId = 'mcp-servers';

export default function RemoteHooksPage() {
  const { t } = useTranslation();
  const [searchParams, setSearchParams] = useSearchParams();
  const [activeTab, setActiveTab] = useState<HooksTabId>(DEFAULT_TAB);
  const [oauthBanner, setOauthBanner] = useState<
    { status: 'success'; name: string } | { status: 'error'; message: string } | null
  >(null);

  useEffect(() => {
    const v = searchParams.get('mcp_oauth');
    if (v !== 'success' && v !== 'error') {
      return;
    }
    if (v === 'success') {
      const name = searchParams.get('name') ?? '';
      setOauthBanner({ status: 'success', name });
    } else {
      const message = searchParams.get('message') ?? t('mcp_servers.oauth_error_banner_default');
      setOauthBanner({ status: 'error', message });
    }
    const next = new URLSearchParams(searchParams);
    next.delete('mcp_oauth');
    next.delete('name');
    next.delete('message');
    setSearchParams(next, { replace: true });
  }, [searchParams, setSearchParams, t]);

  const tabDefs = useMemo(
    () =>
      [
        { id: 'mcp-servers' as const, label: t('mcp_servers.tab_label') },
        { id: 'remote-hooks' as const, label: t('remote_hooks.tab_label') },
        { id: 'discovery' as const, label: t('local_hooks.tab_label') },
      ] as const,
    [t],
  );

  const renderPanel = useCallback((id: HooksTabId) => {
    switch (id) {
      case 'mcp-servers':
        return <McpServersSection />;
      case 'remote-hooks':
        return <RemoteHooksSection />;
      case 'discovery':
        return <LocalHooksSection />;
    }
  }, []);

  const tabs = useMemo(
    () =>
      tabDefs.map(tab => ({
        id: tab.id,
        label: tab.label,
        content: tab.id === activeTab ? renderPanel(tab.id) : null,
      })),
    [tabDefs, activeTab, renderPanel],
  );

  return (
    <Page bodyScroll="auto">
      {oauthBanner && (
        <Panel
          variant="flat"
          className={
            oauthBanner.status === 'success'
              ? 'mb-4 border-emerald-600/30 bg-emerald-950/20 text-sm'
              : 'mb-4 border-red-600/30 bg-red-950/20 text-sm'
          }>
          {oauthBanner.status === 'success'
            ? t('mcp_servers.oauth_success_banner', { name: oauthBanner.name })
            : t('mcp_servers.oauth_error_banner', { message: oauthBanner.message })}
        </Panel>
      )}
      <TabbedPage
        tabs={tabs}
        mountActivePanelOnly
        defaultActiveTab={DEFAULT_TAB}
        activeTab={activeTab}
        onTabChange={id => setActiveTab(id as HooksTabId)}
      />
    </Page>
  );
}
