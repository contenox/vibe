import {
  Button,
  EmptyState,
  ErrorState,
  Input,
  Panel,
  Spinner,
  Table,
  TableCell,
  TableRow,
} from '@contenox/ui';
import { Search, Trash2 } from 'lucide-react';
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useDeleteChain } from '../../../../hooks/useChains';

interface ChainsListProps {
  paths: string[];
  isLoading?: boolean;
  error: Error | null;
  onSelectPath?: (vfsPath: string) => void;
  onCreate?: () => void;
}

export default function ChainsList({
  paths,
  isLoading = false,
  error,
  onSelectPath,
  onCreate,
}: ChainsListProps) {
  const { t } = useTranslation();
  const deleteChain = useDeleteChain();
  const [deletingPath, setDeletingPath] = useState<string | null>(null);
  const [search, setSearch] = useState('');

  const filteredPaths = useMemo(() => {
    if (!search.trim()) return paths;
    const q = search.toLowerCase();
    return paths.filter(p => p.toLowerCase().includes(q));
  }, [paths, search]);

  const handleDelete = async (vfsPath: string) => {
    if (
      !window.confirm(t('chains.confirm_delete', 'Delete this chain file? This cannot be undone.'))
    )
      return;
    setDeletingPath(vfsPath);
    try {
      await deleteChain.mutateAsync(vfsPath);
    } finally {
      setDeletingPath(null);
    }
  };

  if (isLoading) {
    return (
      <div className="flex h-64 items-center justify-center">
        <Spinner size="lg" />
      </div>
    );
  }

  if (error) {
    return (
      <ErrorState title={t('chains.list_error')} error={error} />
    );
  }

  if (!filteredPaths.length) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center p-12">
        <EmptyState
          title={t('chains.list_empty_title')}
          description={t('chains.list_empty_message')}
          orientation="horizontal"
          iconSize="lg"
        />
        {onCreate && (
          <Button variant="primary" onClick={onCreate} className="mt-6">
            {t('chains.create_first_chain')}
          </Button>
        )}
      </div>
    );
  }

  return (
    <div className="flex h-full min-h-0 min-w-0 flex-col p-6">
      <div className="mb-6 flex items-center justify-between">
        <div className="relative max-w-md flex-1">
          <Search className="text-muted-foreground absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2" />
          <Input
            value={search}
            onChange={e => setSearch(e.target.value)}
            placeholder={t('chains.search_placeholder', 'Search by file path...')}
            className="pl-10"
          />
        </div>
        {onCreate && (
          <Button onClick={onCreate} variant="primary">
            {t('chains.create_new')}
          </Button>
        )}
      </div>

      <div className="min-h-0 min-w-0 flex-1 overflow-auto rounded-lg border">
        <Table
          columns={[t('files.form_path', 'Path'), t('common.actions')]}
          className="h-full w-full table-fixed">
          {filteredPaths.map(vfsPath => {
            const isDeleting = deletingPath === vfsPath;

            return (
              <TableRow key={vfsPath} className="hover:bg-secondary/50 transition-colors">
                <TableCell className="break-all font-mono text-sm">{vfsPath}</TableCell>

                <TableCell className="space-x-2">
                  {onSelectPath && (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => onSelectPath(vfsPath)}
                      disabled={isDeleting}>
                      {t('common.edit')}
                    </Button>
                  )}

                  <Button
                    variant="danger"
                    size="sm"
                    onClick={() => handleDelete(vfsPath)}
                    disabled={isDeleting}>
                    {isDeleting ? (
                      <Spinner size="sm" />
                    ) : (
                      <>
                        <Trash2 className="mr-1 h-4 w-4" />
                        {t('common.delete')}
                      </>
                    )}
                  </Button>
                </TableCell>
              </TableRow>
            );
          })}
        </Table>
      </div>

      <div className="text-muted-foreground mt-4 flex items-center justify-between text-sm">
        <span>
          {t('chains.total_chains')}: {paths.length}
          {search && filteredPaths.length !== paths.length && (
            <span>
              {' '}
              ({t('chains.filtered')}: {filteredPaths.length})
            </span>
          )}
        </span>
        <span>{t('chains.default_chains_notice')}</span>
      </div>
    </div>
  );
}
