import { Button, ErrorState, Fill, LoadingState, Page, Section } from '@contenox/ui';
import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';
import {
  useCreatePolicy,
  useDeletePolicy,
  useListPolicies,
  usePolicy,
  useSetActivePolicy,
  useUpdatePolicy,
} from '../../../hooks/usePolicies';
import { useSetupStatus } from '../../../hooks/useSetupStatus';
import type { HITLPolicy } from '../../../lib/types';
import PolicyEditor from './components/PolicyEditor';
import PolicyList from './components/PolicyList';

const EMPTY_POLICY: HITLPolicy = { rules: [] };

export default function HITLPoliciesPage() {
  const { t } = useTranslation();
  const [searchParams, setSearchParams] = useSearchParams();
  const nameParam = searchParams.get('name') ?? '';

  const { data: policyNames = [], isLoading: listLoading, error: listError } = useListPolicies();
  const { data: loadedPolicy, isLoading: policyLoading, error: policyError } = usePolicy(nameParam);
  const { data: setupStatus } = useSetupStatus(true);

  const activePolicyName = setupStatus?.hitlPolicyName ?? '';

  const [jsonDraft, setJsonDraft] = useState<string>('');
  const [pendingName, setPendingName] = useState<string | null>(null);
  const [isNewDraft, setIsNewDraft] = useState(false);

  const editorName = nameParam || pendingName || '';

  const createPolicy = useCreatePolicy();
  const updatePolicy = useUpdatePolicy(editorName);
  const deletePolicy = useDeletePolicy();
  const setActive = useSetActivePolicy();

  useEffect(() => {
    if (nameParam && loadedPolicy) {
      setJsonDraft(JSON.stringify(loadedPolicy, null, 2));
      setPendingName(null);
      setIsNewDraft(false);
    }
  }, [nameParam, loadedPolicy]);

  const handleSelect = (name: string) => {
    setSearchParams({ name }, { replace: true });
    setPendingName(null);
    setIsNewDraft(false);
  };

  const handleCreateNew = () => {
    const name = `hitl-policy-custom-${Date.now()}.json`;
    setPendingName(name);
    setIsNewDraft(true);
    setSearchParams({}, { replace: true });
    setJsonDraft(JSON.stringify(EMPTY_POLICY, null, 2));
  };

  const handleSave = useCallback(async () => {
    let parsed: HITLPolicy;
    try {
      parsed = JSON.parse(jsonDraft) as HITLPolicy;
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      alert(`${t('hitl_policies.invalid_json')}: ${msg}`);
      return;
    }

    if (isNewDraft && pendingName) {
      await createPolicy.mutateAsync({ name: pendingName, policy: parsed });
      setIsNewDraft(false);
      setSearchParams({ name: pendingName }, { replace: true });
      return;
    }

    if (nameParam) {
      await updatePolicy.mutateAsync(parsed);
    }
  }, [jsonDraft, isNewDraft, pendingName, nameParam, createPolicy, updatePolicy, setSearchParams, t]);

  const handleDelete = async (name: string) => {
    if (!confirm(t('hitl_policies.confirm_delete', { name }))) return;
    await deletePolicy.mutateAsync(name);
    if (nameParam === name) setSearchParams({}, { replace: true });
  };

  const handleSetActive = (name: string) => setActive.mutate(name);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
        e.preventDefault();
        void handleSave();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [handleSave]);

  const isLoading = listLoading || (!!nameParam && policyLoading);
  const error = listError ?? policyError ?? null;

  if (isLoading) return <LoadingState message={t('hitl_policies.loading')} />;
  if (error) return <ErrorState error={error} title={t('hitl_policies.loading_error')} />;

  const isSaving = createPolicy.isPending || updatePolicy.isPending;

  return (
    <Page
      bodyScroll="hidden"
      header={
        <Section title={t('hitl_policies.title')} description={t('hitl_policies.description')}>
          <div className="flex items-center gap-2">
            <Button variant="primary" onClick={handleCreateNew}>
              {t('hitl_policies.create_new')}
            </Button>
            {editorName && (
              <Button
                variant="primary"
                onClick={handleSave}
                disabled={isSaving || !editorName}>
                {isSaving ? t('common.saving') : t('common.save')}
              </Button>
            )}
          </div>
        </Section>
      }>
      <Fill className="flex min-h-0 min-w-0">
        <PolicyList
          names={policyNames}
          activeName={activePolicyName}
          selectedName={editorName}
          onSelect={handleSelect}
          onSetActive={handleSetActive}
          onDelete={handleDelete}
        />
        {editorName ? (
          <PolicyEditor
            value={jsonDraft || JSON.stringify(EMPTY_POLICY, null, 2)}
            onChange={setJsonDraft}
            className="flex-1"
          />
        ) : (
          <div className="flex flex-1 items-center justify-center text-sm text-neutral-500">
            {t('hitl_policies.select_or_create')}
          </div>
        )}
      </Fill>
    </Page>
  );
}
