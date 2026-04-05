import { Button, Form, FormField, Input, P, Panel } from '@contenox/ui';
import { t } from 'i18next';
import React from 'react';
import { MCPServer } from '../../../../lib/types';

export type McpServerFormProps = {
  editingServer: MCPServer | null;
  onCancel: () => void;
  onSubmit: (e: React.FormEvent) => void;
  isPending: boolean;
  error: boolean;
  name: string;
  setName: (v: string) => void;
  transport: string;
  setTransport: (v: string) => void;
  command: string;
  setCommand: (v: string) => void;
  argsText: string;
  setArgsText: (v: string) => void;
  url: string;
  setUrl: (v: string) => void;
  connectTimeoutSeconds: number;
  setConnectTimeoutSeconds: (v: number) => void;
  authType: string;
  setAuthType: (v: string) => void;
  authEnvKey: string;
  setAuthEnvKey: (v: string) => void;
  authToken: string;
  setAuthToken: (v: string) => void;
  headers: Record<string, string>;
  setHeaders: React.Dispatch<React.SetStateAction<Record<string, string>>>;
  injectParams: Record<string, string>;
  setInjectParams: React.Dispatch<React.SetStateAction<Record<string, string>>>;
};

export default function McpServerForm({
  editingServer,
  onCancel,
  onSubmit,
  isPending,
  error,
  name,
  setName,
  transport,
  setTransport,
  command,
  setCommand,
  argsText,
  setArgsText,
  url,
  setUrl,
  connectTimeoutSeconds,
  setConnectTimeoutSeconds,
  authType,
  setAuthType,
  authEnvKey,
  setAuthEnvKey,
  authToken,
  setAuthToken,
  headers,
  setHeaders,
  injectParams,
  setInjectParams,
}: McpServerFormProps) {
  const handleHeaderChange = (key: string, value: string) => {
    setHeaders(prev => ({ ...prev, [key]: value }));
  };
  const removeHeader = (key: string) => {
    setHeaders(prev => {
      const next = { ...prev };
      delete next[key];
      return next;
    });
  };
  const addHeaderField = () => {
    setHeaders(prev => ({ ...prev, [`header-${Date.now()}`]: '' }));
  };

  const handleInjectChange = (key: string, value: string) => {
    setInjectParams(prev => ({ ...prev, [key]: value }));
  };
  const removeInject = (key: string) => {
    setInjectParams(prev => {
      const next = { ...prev };
      delete next[key];
      return next;
    });
  };
  const addInjectField = () => {
    setInjectParams(prev => ({ ...prev, [`inject-${Date.now()}`]: '' }));
  };

  const nameOk = name.trim().length > 0;
  const timeoutOk = connectTimeoutSeconds > 0;
  let transportOk = false;
  if (transport === 'stdio') {
    transportOk = command.trim().length > 0;
  } else if (transport === 'sse' || transport === 'http') {
    transportOk = url.trim().length > 0;
  }

  const isValidForm = nameOk && timeoutOk && transportOk;

  return (
    <Form
      title={
        editingServer ? t('mcp_servers.edit_title') : t('mcp_servers.create_title')
      }
      onSubmit={onSubmit}
      actions={
        <>
          <Button type="button" variant="secondary" onClick={onCancel} disabled={isPending}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" variant="primary" disabled={isPending || !isValidForm}>
            {isPending ? t('common.saving') : editingServer ? t('common.update') : t('common.create')}
          </Button>
        </>
      }>
      {error && (
        <Panel variant="error" className="mb-4">
          {t('mcp_servers.save_error')}
        </Panel>
      )}

      <P variant="muted" className="mb-4 text-sm">
        {t('mcp_servers.cli_hint')}
      </P>

      <FormField label={t('mcp_servers.name')} required>
        <Input
          value={name}
          onChange={e => setName(e.target.value)}
          placeholder={t('mcp_servers.name_placeholder')}
          disabled={isPending}
        />
      </FormField>

      <FormField label={t('mcp_servers.transport')} required>
        <select
          value={transport}
          onChange={e => setTransport(e.target.value)}
          disabled={isPending}
          className="border-input bg-background w-full rounded border px-3 py-2 text-sm">
          <option value="http">{t('mcp_servers.transport_http')}</option>
          <option value="sse">{t('mcp_servers.transport_sse')}</option>
          <option value="stdio">{t('mcp_servers.transport_stdio')}</option>
        </select>
      </FormField>

      {transport === 'stdio' && (
        <>
          <FormField label={t('mcp_servers.command')} required>
            <Input
              value={command}
              onChange={e => setCommand(e.target.value)}
              placeholder={t('mcp_servers.command_placeholder')}
              disabled={isPending}
            />
          </FormField>
          <FormField label={t('mcp_servers.args')}>
            <textarea
              value={argsText}
              onChange={e => setArgsText(e.target.value)}
              placeholder={t('mcp_servers.args_placeholder')}
              disabled={isPending}
              rows={4}
              className="border-input bg-background w-full rounded border px-3 py-2 font-mono text-sm"
            />
            <P variant="muted" className="mt-1 text-xs">
              {t('mcp_servers.args_help')}
            </P>
          </FormField>
        </>
      )}

      {(transport === 'sse' || transport === 'http') && (
        <FormField label={t('mcp_servers.url')} required>
          <Input
            type="url"
            value={url}
            onChange={e => setUrl(e.target.value)}
            placeholder={t('mcp_servers.url_placeholder')}
            disabled={isPending}
          />
        </FormField>
      )}

      <FormField label={t('mcp_servers.connect_timeout_seconds')} required>
        <Input
          type="number"
          min={1}
          max={600}
          value={connectTimeoutSeconds}
          onChange={e => setConnectTimeoutSeconds(Math.max(1, Number(e.target.value) || 1))}
          disabled={isPending}
        />
      </FormField>

      <FormField label={t('mcp_servers.auth')}>
        <select
          value={authType || ''}
          onChange={e => setAuthType(e.target.value)}
          disabled={isPending || transport === 'stdio'}
          className="border-input bg-background w-full rounded border px-3 py-2 text-sm">
          <option value="">{t('mcp_servers.auth_none')}</option>
          <option value="bearer">{t('mcp_servers.auth_bearer')}</option>
          <option value="oauth">{t('mcp_servers.auth_oauth')}</option>
        </select>
      </FormField>

      {authType === 'oauth' && (transport === 'sse' || transport === 'http') && (
        <P variant="muted" className="mb-4 text-sm">
          {t('mcp_servers.oauth_form_help')}
        </P>
      )}

      {authType === 'bearer' && (transport === 'sse' || transport === 'http') && (
        <>
          <FormField label={t('mcp_servers.auth_env_key')}>
            <Input
              value={authEnvKey}
              onChange={e => setAuthEnvKey(e.target.value)}
              placeholder="MCP_TOKEN"
              disabled={isPending}
            />
            <P variant="muted" className="mt-1 text-xs">
              {t('mcp_servers.auth_env_key_help')}
            </P>
          </FormField>
          <FormField label={t('mcp_servers.auth_token')}>
            <Input
              type="password"
              autoComplete="new-password"
              value={authToken}
              onChange={e => setAuthToken(e.target.value)}
              placeholder={t('mcp_servers.auth_token_placeholder')}
              disabled={isPending}
            />
            <Panel variant="flat" className="mt-2 text-xs">
              {t('mcp_servers.auth_token_warning')}
            </Panel>
          </FormField>
        </>
      )}

      {(transport === 'sse' || transport === 'http') && (
        <FormField label={t('remote_hooks.headers')}>
          <div className="space-y-2">
            {Object.entries(headers).map(([key, value]) => (
              <div key={key} className="flex gap-2">
                <Input
                  value={key}
                  onChange={e => handleHeaderChange(e.target.value, value)}
                  placeholder="Header key"
                  className="flex-1"
                  disabled={isPending}
                />
                <Input
                  value={value}
                  onChange={e => handleHeaderChange(key, e.target.value)}
                  placeholder="Header value"
                  className="flex-1"
                  disabled={isPending}
                />
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => removeHeader(key)}
                  disabled={isPending}>
                  {t('common.remove')}
                </Button>
              </div>
            ))}
          </div>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={addHeaderField}
            disabled={isPending}
            className="mt-2">
            {t('remote_hooks.add_header')}
          </Button>
        </FormField>
      )}

      <FormField label={t('mcp_servers.inject_params')}>
        <P variant="muted" className="mb-2 text-xs">
          {t('mcp_servers.inject_help')}
        </P>
        <div className="space-y-2">
          {Object.entries(injectParams).map(([key, value]) => (
            <div key={key} className="flex gap-2">
              <Input
                value={key}
                onChange={e => handleInjectChange(e.target.value, value)}
                placeholder={t('mcp_servers.inject_key_placeholder')}
                className="flex-1"
                disabled={isPending}
              />
              <Input
                value={value}
                onChange={e => handleInjectChange(key, e.target.value)}
                placeholder={t('mcp_servers.inject_value_placeholder')}
                className="flex-1"
                disabled={isPending}
              />
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => removeInject(key)}
                disabled={isPending}>
                {t('common.remove')}
              </Button>
            </div>
          ))}
        </div>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={addInjectField}
          disabled={isPending}
          className="mt-2">
          {t('mcp_servers.add_inject')}
        </Button>
      </FormField>
    </Form>
  );
}
