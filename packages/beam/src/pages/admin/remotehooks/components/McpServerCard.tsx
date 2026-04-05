import { Button, Card, P } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { MCPServer } from '../../../../lib/types';

type McpServerCardProps = {
  server: MCPServer;
  onEdit: (server: MCPServer) => void;
  onDelete: (id: string) => void;
  isDeleting: boolean;
  /** OAuth PKCE: open provider in a new tab (Beam callback stores the token). */
  onStartOAuth?: () => void | Promise<void>;
  oauthStarting?: boolean;
};

function summaryLine(s: MCPServer): string {
  if (s.transport === 'stdio') {
    const args = s.args?.length ? ` ${s.args.join(' ')}` : '';
    return `${s.command || ''}${args}`.trim() || '—';
  }
  return s.url || '—';
}

export default function McpServerCard({
  server,
  onEdit,
  onDelete,
  isDeleting,
  onStartOAuth,
  oauthStarting,
}: McpServerCardProps) {
  const { t } = useTranslation();

  return (
    <Card variant="surface">
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <P className="font-semibold">{server.name}</P>
          <P className="text-muted text-sm">
            {t('mcp_servers.transport_label')}: {server.transport}
          </P>
          <P className="text-muted truncate text-xs" title={summaryLine(server)}>
            {summaryLine(server)}
          </P>
          <P className="text-muted text-xs">
            {t('mcp_servers.connect_timeout_seconds')}: {server.connectTimeoutSeconds ?? 30}s
          </P>
          {server.headers && Object.keys(server.headers).length > 0 && (
            <P className="text-muted text-xs">
              {t('remote_hooks.headers_count', { count: Object.keys(server.headers).length })}
            </P>
          )}
          {server.authType === 'oauth' && (
            <P className="text-muted text-xs">{t('mcp_servers.oauth_badge')}</P>
          )}
        </div>
        <div className="flex shrink-0 flex-wrap justify-end gap-2">
          {server.authType === 'oauth' &&
            (server.transport === 'sse' || server.transport === 'http') &&
            onStartOAuth && (
              <Button
                type="button"
                variant="secondary"
                size="sm"
                disabled={oauthStarting}
                onClick={() => void onStartOAuth()}>
                {oauthStarting ? t('mcp_servers.oauth_opening') : t('mcp_servers.oauth_sign_in')}
              </Button>
            )}
          <Button type="button" variant="ghost" size="sm" onClick={() => onEdit(server)}>
            {t('common.edit')}
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="text-error"
            disabled={isDeleting}
            onClick={() => onDelete(server.id)}>
            {isDeleting ? t('common.deleting') : t('common.delete')}
          </Button>
        </div>
      </div>
    </Card>
  );
}
