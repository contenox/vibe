import { ErrorState, GridLayout, LoadingState, Panel, Section, Span } from '@contenox/ui';
import { t } from 'i18next';
import { useState } from 'react';
import {
  useCreateMcpServer,
  useDeleteMcpServer,
  useMcpServers,
  useStartMcpOAuth,
  useUpdateMcpServer,
} from '../../../../hooks/useMcpServers';
import { MCPServer } from '../../../../lib/types';
import McpServerCard from './McpServerCard';
import McpServerForm from './McpServerForm';

function parseArgsLines(text: string): string[] {
  return text
    .split(/\r?\n/)
    .map(s => s.trim())
    .filter(Boolean);
}

function buildPayload(
  name: string,
  transport: string,
  command: string,
  argsText: string,
  url: string,
  connectTimeoutSeconds: number,
  authType: string,
  authEnvKey: string,
  authToken: string,
  headers: Record<string, string>,
  injectParams: Record<string, string>,
): Partial<MCPServer> {
  const h =
    Object.keys(headers).length > 0
      ? Object.fromEntries(Object.entries(headers).filter(([, v]) => v !== ''))
      : undefined;
  const inj =
    Object.keys(injectParams).length > 0
      ? Object.fromEntries(Object.entries(injectParams).filter(([, v]) => v !== ''))
      : undefined;

  const base: Partial<MCPServer> = {
    name: name.trim(),
    transport,
    connectTimeoutSeconds,
    injectParams: inj && Object.keys(inj).length > 0 ? inj : undefined,
  };

  if (transport === 'stdio') {
    base.command = command.trim();
    const args = parseArgsLines(argsText);
    base.args = args.length > 0 ? args : undefined;
    base.url = '';
    base.authType = undefined;
    base.authEnvKey = undefined;
    base.authToken = undefined;
    base.headers = undefined;
  } else {
    base.headers = h && Object.keys(h).length > 0 ? h : undefined;
    base.url = url.trim();
    base.command = '';
    base.args = undefined;
    if (authType === 'bearer') {
      base.authType = 'bearer';
      base.authEnvKey = authEnvKey.trim() || undefined;
      base.authToken = authToken.trim() || undefined;
    } else if (authType === 'oauth') {
      base.authType = 'oauth';
      base.authEnvKey = undefined;
      base.authToken = undefined;
    } else {
      base.authType = undefined;
      base.authEnvKey = undefined;
      base.authToken = undefined;
    }
  }

  return base;
}

export default function McpServersSection() {
  const [editingServer, setEditingServer] = useState<MCPServer | null>(null);
  const [name, setName] = useState('');
  const [transport, setTransport] = useState('http');
  const [command, setCommand] = useState('');
  const [argsText, setArgsText] = useState('');
  const [url, setUrl] = useState('');
  const [connectTimeoutSeconds, setConnectTimeoutSeconds] = useState(30);
  const [authType, setAuthType] = useState('');
  const [authEnvKey, setAuthEnvKey] = useState('');
  const [authToken, setAuthToken] = useState('');
  const [headers, setHeaders] = useState<Record<string, string>>({});
  const [injectParams, setInjectParams] = useState<Record<string, string>>({});

  const { data: servers, isLoading, error, refetch } = useMcpServers({ limit: 100 });
  const createMutation = useCreateMcpServer();
  const updateMutation = useUpdateMcpServer();
  const deleteMutation = useDeleteMcpServer();
  const startOAuthMutation = useStartMcpOAuth();

  const resetForm = () => {
    setEditingServer(null);
    setName('');
    setTransport('http');
    setCommand('');
    setArgsText('');
    setUrl('');
    setConnectTimeoutSeconds(30);
    setAuthType('');
    setAuthEnvKey('');
    setAuthToken('');
    setHeaders({});
    setInjectParams({});
  };

  const handleEdit = (srv: MCPServer) => {
    setEditingServer(srv);
    setName(srv.name);
    setTransport(srv.transport || 'http');
    setCommand(srv.command || '');
    setArgsText(srv.args?.length ? srv.args.join('\n') : '');
    setUrl(srv.url || '');
    setConnectTimeoutSeconds(srv.connectTimeoutSeconds > 0 ? srv.connectTimeoutSeconds : 30);
    setAuthType(
      srv.authType === 'bearer' ? 'bearer' : srv.authType === 'oauth' ? 'oauth' : '',
    );
    setAuthEnvKey(srv.authEnvKey || '');
    setAuthToken(srv.authToken || '');
    setHeaders(srv.headers && Object.keys(srv.headers).length > 0 ? { ...srv.headers } : {});
    setInjectParams(
      srv.injectParams && Object.keys(srv.injectParams).length > 0
        ? { ...srv.injectParams }
        : {},
    );
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const payload = buildPayload(
      name,
      transport,
      command,
      argsText,
      url,
      connectTimeoutSeconds,
      authType,
      authEnvKey,
      authToken,
      headers,
      injectParams,
    );

    if (editingServer) {
      updateMutation.mutate(
        { id: editingServer.id, data: { ...payload, id: editingServer.id } },
        { onSuccess: resetForm },
      );
    } else {
      createMutation.mutate(payload, { onSuccess: resetForm });
    }
  };

  const handleDelete = async (id: string) => {
    if (window.confirm(t('mcp_servers.delete_confirm'))) {
      await deleteMutation.mutateAsync(id);
    }
  };

  if (isLoading) {
    return <LoadingState message={t('mcp_servers.list_loading')} />;
  }

  if (error) {
    return <ErrorState error={error} onRetry={refetch} title={t('mcp_servers.list_error')} />;
  }

  return (
    <GridLayout variant="body" columns={2} responsive={{ base: 1, lg: 2 }} className="gap-6">
      <Section title={t('mcp_servers.manage_title')} className="space-y-4">
        <Span variant="muted" className="text-sm">
          {t('mcp_servers.count', { count: servers?.length ?? 0 })}
        </Span>
        <p className="text-muted text-sm">{t('mcp_servers.description')}</p>

        <div className="max-h-[600px] space-y-4 overflow-y-auto">
          {servers && servers.length > 0 ? (
            servers.map(srv => (
              <McpServerCard
                key={srv.id}
                server={srv}
                onEdit={handleEdit}
                onDelete={handleDelete}
                isDeleting={deleteMutation.isPending}
                onStartOAuth={async () => {
                  const { authorizationUrl } = await startOAuthMutation.mutateAsync({
                    id: srv.id,
                    redirectBase: window.location.origin,
                  });
                  window.open(authorizationUrl, '_blank', 'noopener,noreferrer');
                }}
                oauthStarting={
                  startOAuthMutation.isPending && startOAuthMutation.variables?.id === srv.id
                }
              />
            ))
          ) : (
            <Panel variant="bordered" className="py-12 text-center">
              <div className="text-muted-foreground">
                <Span className="mb-2 block">{t('mcp_servers.list_empty_title')}</Span>
                <Span variant="muted" className="text-sm">
                  {t('mcp_servers.list_empty_description')}
                </Span>
              </div>
            </Panel>
          )}
        </div>
      </Section>

      <Section>
        <McpServerForm
          editingServer={editingServer}
          onCancel={resetForm}
          onSubmit={handleSubmit}
          isPending={editingServer ? updateMutation.isPending : createMutation.isPending}
          error={createMutation.isError || updateMutation.isError}
          name={name}
          setName={setName}
          transport={transport}
          setTransport={setTransport}
          command={command}
          setCommand={setCommand}
          argsText={argsText}
          setArgsText={setArgsText}
          url={url}
          setUrl={setUrl}
          connectTimeoutSeconds={connectTimeoutSeconds}
          setConnectTimeoutSeconds={setConnectTimeoutSeconds}
          authType={authType}
          setAuthType={setAuthType}
          authEnvKey={authEnvKey}
          setAuthEnvKey={setAuthEnvKey}
          authToken={authToken}
          setAuthToken={setAuthToken}
          headers={headers}
          setHeaders={setHeaders}
          injectParams={injectParams}
          setInjectParams={setInjectParams}
        />
      </Section>
    </GridLayout>
  );
}
