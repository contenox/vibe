import { Button, Card, P } from '@contenox/ui';
import { t } from 'i18next';
import { RemoteHook } from '../../../../lib/types';

type RemoteHookCardProps = {
  hook: RemoteHook;
  onEdit: (hook: RemoteHook) => void;
  onDelete: (id: string) => void;
  isDeleting: boolean;
};

export default function RemoteHookCard({
  hook,
  onEdit,
  onDelete,
  isDeleting,
}: RemoteHookCardProps) {
  return (
    <Card variant="surface">
      <div className="flex items-start justify-between">
        <div>
          <P className="font-semibold">{hook.name}</P>
          <P className="text-muted text-sm">{hook.endpointUrl}</P>
          <P className="text-muted text-xs">
            {t('remote_hooks.timeout')}: {hook.timeoutMs}ms
          </P>
          {hook.headers && Object.keys(hook.headers).length > 0 && (
            <P className="text-muted text-xs">
              {t('remote_hooks.headers_count', { count: Object.keys(hook.headers).length })}
            </P>
          )}
        </div>
        <div className="flex gap-2">
          <Button variant="ghost" size="sm" onClick={() => onEdit(hook)}>
            {t('common.edit')}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            className="text-error"
            disabled={isDeleting}
            onClick={() => onDelete(hook.id)}>
            {isDeleting ? t('common.deleting') : t('common.delete')}
          </Button>
        </div>
      </div>
    </Card>
  );
}
