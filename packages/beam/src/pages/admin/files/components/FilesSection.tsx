import {
  Button,
  EmptyState,
  Form,
  FormField,
  GridLayout,
  H2,
  Input,
  Panel,
  Section,
  Spinner,
  Table,
  TableCell,
  TableRow,
} from '@contenox/ui';
import React, { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { useCreateFile, useDeleteFile, useListFiles } from '../../../../hooks/useFiles';
import { isChainLikeVfsPath } from '../../../../lib/chainPaths';
import { api } from '../../../../lib/api';

function formatBytes(bytes: number, decimals = 2): string {
  if (!+bytes) return '0 Bytes';
  const k = 1024;
  const dm = decimals < 0 ? 0 : decimals;
  const sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB', 'ZB', 'YB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return `${parseFloat((bytes / Math.pow(k, i)).toFixed(dm))} ${sizes[i]}`;
}

export default function FilesSection() {
  const { t } = useTranslation();
  const [selectedFile, setSelectedFile] = useState<File | null>(null);
  const [uploadPath, setUploadPath] = useState('');
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [uploadError, setUploadError] = useState<string | undefined>(undefined);
  const [deleteError, setDeleteError] = useState<string | undefined>(undefined);

  const { data: files, isLoading: isLoadingFiles, error: filesError } = useListFiles();
  const createFileMutation = useCreateFile();
  const deleteFileMutation = useDeleteFile();

  useEffect(() => {
    if (uploadError) {
      const timer = setTimeout(() => setUploadError(undefined), 5000);
      return () => clearTimeout(timer);
    }
  }, [uploadError]);

  useEffect(() => {
    if (deleteError) {
      const timer = setTimeout(() => setDeleteError(undefined), 5000);
      return () => clearTimeout(timer);
    }
  }, [deleteError]);

  const handleFileChange = (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    setSelectedFile(file || null);
    setUploadError(undefined);
    if (file && !uploadPath) {
      setUploadPath(file.name);
    }
  };

  const handleUploadSubmit = (event: React.FormEvent) => {
    event.preventDefault();
    if (!selectedFile) return;

    const formData = new FormData();
    formData.append('file', selectedFile);
    formData.append('path', uploadPath);

    setUploadError(undefined);
    createFileMutation.mutate(formData, {
      onSuccess: () => {
        setSelectedFile(null);
        setUploadPath('');
      },
      onError: error => {
        const message = error.message || t('errors.generic_upload');
        setUploadError(message);
      },
    });
  };

  const handleDeleteClick = (id: string) => {
    setDeleteError(undefined);
    setDeletingId(id);
    deleteFileMutation.mutate(id, {
      onSettled: () => {
        setDeletingId(null);
      },
      onError: error => {
        const message = error.message || t('errors.generic_delete');
        setDeleteError(message);
      },
    });
  };

  const renderFileList = () => {
    if (isLoadingFiles) {
      return (
        <Section className="flex items-center justify-center py-10">
          <Spinner size="lg" />
        </Section>
      );
    }

    if (filesError) {
      return (
        <Panel variant="error" title={t('files.list_error_title')}>
          {filesError.message || t('errors.generic_fetch')}
        </Panel>
      );
    }

    if (!files || files.length === 0) {
      return (
        <EmptyState
          title={t('files.list_empty_title')}
          description={t('files.list_empty_message')}
        />
      );
    }

    return (
      <Table columns={[t('common.path'), t('common.type'), t('common.size'), t('common.actions')]}>
        {files.map(file => {
          const isDeleting = deletingId === file.id;
          const chainEdit =
            isChainLikeVfsPath(file.path) &&
            `/chains?path=${encodeURIComponent(file.path)}`;
          return (
            <TableRow key={file.id}>
              <TableCell className="break-all">{file.path}</TableCell>
              <TableCell>{file.contentType}</TableCell>
              <TableCell>{formatBytes(file.size)}</TableCell>
              <TableCell className="space-x-2">
                {chainEdit && (
                  <Link to={chainEdit}>
                    <Button variant="secondary" size="sm" type="button">
                      {t('chains.edit_chain', 'Edit chain')}
                    </Button>
                  </Link>
                )}
                <Button
                  variant="accent"
                  size="sm"
                  onClick={() => handleDeleteClick(file.id)}
                  disabled={isDeleting || deleteFileMutation.isPending}>
                  {isDeleting ? <Spinner size="sm" /> : t('common.delete')}
                </Button>
                <a href={api.getDownloadFileUrl(file.id)} download={file.path}>
                  <Button variant="secondary" size="sm">
                    {t('common.download')}
                  </Button>
                </a>
              </TableCell>
            </TableRow>
          );
        })}
      </Table>
    );
  };

  return (
    <GridLayout variant="body">
      <Section className="overflow-hidden">
        <H2 className="mb-4">{t('files.list_title')}</H2>
        {deleteError && (
          <Panel variant="error" className="mb-4">
            {deleteError}
          </Panel>
        )}
        <Panel className="overflow-auto">{renderFileList()}</Panel>
      </Section>

      <Section>
        <Form
          title={t('files.upload_title')}
          onSubmit={handleUploadSubmit}
          error={uploadError}
          actions={
            <Button
              type="submit"
              variant="primary"
              disabled={!selectedFile || createFileMutation.isPending}>
              {createFileMutation.isPending ? <Spinner size="sm" /> : t('files.upload_action')}
            </Button>
          }>
          <FormField label={t('files.form_select_file')} required>
            <Input type="file" onChange={handleFileChange} />
          </FormField>
          <FormField label={t('files.form_path')}>
            <Input
              value={uploadPath}
              onChange={e => setUploadPath(e.target.value)}
              placeholder={t('files.form_path_placeholder')}
            />
          </FormField>
        </Form>
      </Section>
    </GridLayout>
  );
}
