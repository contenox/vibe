import {
  EmptyState,
  LoadingState,
  Panel,
  Section,
  Table,
  TableCell,
  TableRow,
} from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import type { BackendRuntimeState } from '../../../../lib/types';

type RuntimeStateSectionProps = {
  data: BackendRuntimeState[] | undefined;
  isLoading: boolean;
  isError: boolean;
  errorMessage?: string;
};

export default function RuntimeStateSection({
  data,
  isLoading,
  isError,
  errorMessage,
}: RuntimeStateSectionProps) {
  const { t } = useTranslation();

  return (
    <Section title={t('state.runtime_title')} description={t('state.runtime_intro')}>
      {isLoading && (
        <LoadingState />
      )}
      {isError && (
        <Panel variant="error" className="mb-4">
          {errorMessage || t('state.runtime_error')}
        </Panel>
      )}
      {!isLoading && !isError && data && data.length === 0 && (
        <EmptyState
          title={t('state.runtime_empty_title')}
          description={t('state.runtime_empty_desc')}
          orientation="horizontal"
          iconSize="lg"
        />
      )}
      {!isLoading && !isError && data && data.length > 0 && (
        <Table
          columns={[
            t('state.col_backend'),
            t('state.col_type'),
            t('state.col_url'),
            t('state.col_error'),
            t('state.col_models'),
          ]}>
          {data.map(row => (
            <TableRow key={row.id}>
              <TableCell className="font-medium">{row.name}</TableCell>
              <TableCell>{row.backend?.type ?? '—'}</TableCell>
              <TableCell className="max-w-[220px] truncate font-mono text-xs">
                {row.backend?.baseUrl ?? '—'}
              </TableCell>
              <TableCell className="max-w-[240px] text-sm text-destructive">
                {row.error?.trim() ? row.error : '—'}
              </TableCell>
              <TableCell>{row.pulledModels?.length ?? row.models?.length ?? 0}</TableCell>
            </TableRow>
          ))}
        </Table>
      )}
    </Section>
  );
}
