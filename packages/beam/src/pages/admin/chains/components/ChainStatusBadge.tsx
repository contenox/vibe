import { Badge } from '@contenox/ui';
import { useTranslation } from 'react-i18next';

interface ChainStatusBadgeProps {
  isDefault: boolean;
}

export default function ChainStatusBadge({ isDefault }: ChainStatusBadgeProps) {
  const { t } = useTranslation();

  return isDefault ? (
    <Badge variant="success">{t('chains.status_default')}</Badge>
  ) : (
    <Badge variant="default">{t('chains.status_custom')}</Badge>
  );
}
