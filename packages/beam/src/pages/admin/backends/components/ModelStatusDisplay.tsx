import { Badge, Span } from '@contenox/ui';
import { useTranslation } from 'react-i18next';

type ModelStatusDisplayProps = {
  modelName: string;
};

export function ModelStatusDisplay({ modelName }: ModelStatusDisplayProps) {
  const { t } = useTranslation();

  return (
    <div className="flex items-center justify-between py-1">
      <Span className="text-sm font-medium">{modelName}</Span>
      <Badge variant="success" size="sm">
        {t('backends.status.available')}
      </Badge>
    </div>
  );
}
