import { Badge, Label, P } from '@contenox/ui';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ResourceCard } from '../../../../components/ResourceCard';
import { Backend } from '../../../../lib/types';
import { ModelStatusDisplay } from './ModelStatusDisplay';

type BackendCardProps = {
  backend: Backend;
  onEdit: (backend: Backend) => void;
  onDelete: (id: string) => Promise<void>;
};

export function BackendCard({ backend, onEdit, onDelete }: BackendCardProps) {
  const { t } = useTranslation();
  const [deletingBackendId, setDeletingBackendId] = useState<string | null>(null);
  const observedModels = backend.pulledModels ?? [];

  const handleDelete = async (id: string) => {
    setDeletingBackendId(id);
    try {
      await onDelete(id);
    } finally {
      setDeletingBackendId(null);
    }
  };

  return (
    <ResourceCard
      title={backend.name}
      subtitle={backend.baseUrl}
      status={backend.error ? 'error' : 'default'}
      actions={{
        edit: () => onEdit(backend),
        delete: () => handleDelete(backend.id),
      }}
      isLoading={deletingBackendId === backend.id}>
      <div className="mb-3 flex items-center gap-2">
        <Badge variant={backend.error ? 'error' : 'default'} size="sm">
          {backend.type}
        </Badge>
        {backend.error && (
          <Badge variant="error" size="sm">
            {t('backends.error')}
          </Badge>
        )}
      </div>

      {backend.error && (
        <div className="bg-error/10 border-error/20 mb-3 rounded-lg border p-3">
          <P variant="muted" className="text-error text-sm">
            {backend.error}
          </P>
        </div>
      )}

      {observedModels.length > 0 && (
        <div>
          <Label className="mb-2 block text-sm font-medium">
            {t('backends.observed_models_title')} ({observedModels.length})
          </Label>
          <div className="max-h-60 space-y-2 overflow-y-auto">
            {observedModels.map(model => (
              <ModelStatusDisplay
                key={model.model}
                modelName={model.model}
              />
            ))}
          </div>
        </div>
      )}
    </ResourceCard>
  );
}
