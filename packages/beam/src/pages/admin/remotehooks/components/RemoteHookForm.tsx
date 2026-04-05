import { Button, Form, FormField, Input, Panel } from '@contenox/ui';
import { t } from 'i18next';
import React from 'react';
import { InjectionArg, RemoteHook } from '../../../../lib/types';

type RemoteHookFormProps = {
  editingHook: RemoteHook | null;
  onCancel: () => void;
  onSubmit: (e: React.FormEvent) => void;
  isPending: boolean;
  error: boolean;
  name: string;
  setName: (name: string) => void;
  endpointUrl: string;
  setEndpointUrl: (url: string) => void;
  timeoutMs: number;
  setTimeoutMs: (ms: number) => void;
  headers: Record<string, string>;
  setHeaders: React.Dispatch<React.SetStateAction<Record<string, string>>>;
  properties: InjectionArg;
  setProperties: React.Dispatch<React.SetStateAction<InjectionArg>>;
};

export default function RemoteHookForm({
  editingHook,
  onCancel,
  onSubmit,
  isPending,
  error,
  name,
  setName,
  endpointUrl,
  setEndpointUrl,
  timeoutMs,
  setTimeoutMs,
  headers,
  setHeaders,
  properties,
  setProperties,
}: RemoteHookFormProps) {
  const handleHeaderChange = (key: string, value: string) => {
    setHeaders(prev => ({ ...prev, [key]: value }));
  };

  const removeHeader = (key: string) => {
    setHeaders(prev => {
      const newHeaders = { ...prev };
      delete newHeaders[key];
      return newHeaders;
    });
  };

  const addHeaderField = () => {
    const newKey = `header-${Date.now()}`;
    setHeaders(prev => ({ ...prev, [newKey]: '' }));
  };

  const isValidForm = name.trim() && endpointUrl.trim() && timeoutMs > 0;

  return (
    <Form
      title={editingHook ? t('remote_hooks.edit_title') : t('remote_hooks.create_title')}
      onSubmit={onSubmit}
      actions={
        <>
          <Button type="button" variant="secondary" onClick={onCancel} disabled={isPending}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" variant="primary" disabled={isPending || !isValidForm}>
            {isPending ? t('common.saving') : editingHook ? t('common.update') : t('common.create')}
          </Button>
        </>
      }>
      {error && (
        <Panel variant="error" className="mb-4">
          {t('remote_hooks.save_error')}
        </Panel>
      )}

      <FormField label={t('remote_hooks.name')} required>
        <Input
          value={name}
          onChange={e => setName(e.target.value)}
          placeholder={t('remote_hooks.name_placeholder')}
          disabled={isPending}
        />
      </FormField>

      <FormField label={t('remote_hooks.endpoint_url')} required>
        <Input
          type="url"
          value={endpointUrl}
          onChange={e => setEndpointUrl(e.target.value)}
          placeholder="https://example.com/webhook"
          disabled={isPending}
        />
      </FormField>

      <FormField label={t('remote_hooks.timeout_ms')} required>
        <Input
          type="number"
          min="100"
          max="30000"
          value={timeoutMs}
          onChange={e => setTimeoutMs(Math.max(100, Number(e.target.value)))}
          disabled={isPending}
        />
      </FormField>

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

      <div className="mt-6 border-t pt-4">
        <h3 className="mb-3 text-sm font-medium">{t('remote_hooks.properties')}</h3>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <FormField label={t('remote_hooks.arg_name')}>
            <Input
              placeholder={t('remote_hooks.arg_name_placeholder')}
              value={properties.name}
              onChange={e => setProperties(p => ({ ...p, name: e.target.value }))}
              disabled={isPending}
            />
          </FormField>
          <FormField label={t('remote_hooks.arg_value')}>
            <Input
              placeholder={t('remote_hooks.arg_value_placeholder')}
              value={String(properties.value)}
              onChange={e => setProperties(p => ({ ...p, value: e.target.value }))}
              disabled={isPending}
            />
          </FormField>
          <FormField label={t('remote_hooks.arg_location')}>
            <select
              value={properties.in}
              onChange={e => {
                const v = e.target.value;
                if (v === 'body' || v === 'query' || v === 'path') {
                  setProperties(p => ({ ...p, in: v }));
                }
              }}
              disabled={isPending}
              className="border-input bg-background w-full rounded border px-3 py-2 text-sm">
              <option value="path">Path</option>
              <option value="query">Query</option>
              <option value="body">Body</option>
            </select>
          </FormField>
        </div>
      </div>
    </Form>
  );
}
