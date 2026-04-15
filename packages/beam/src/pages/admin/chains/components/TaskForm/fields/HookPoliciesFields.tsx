import { Button, Collapsible, Input, Label, P, Span } from '@contenox/ui';
import { Plus, Trash2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import type { ExecuteConfig, FormTask } from '../../../../../../lib/types';

interface HookPoliciesFieldsProps {
  task: FormTask;
  onChange: (updates: Partial<FormTask>) => void;
}

/**
 * Edits `execute_config.hook_policies`. Three rendering tiers:
 *   1. Typed form for `local_shell` (always shown — most-common footgun).
 *   2. Typed form for `local_fs` (always shown).
 *   3. One sub-section per OTHER hook name found in hook_policies, plus an
 *      "Add hook" button for declaring a new hook with an empty policy bag.
 *
 * Each sub-section omits its own JSON branch when all fields are empty, and
 * the whole `hook_policies` object is dropped from `execute_config` when no
 * sub-section has any populated value. Same omit-empty discipline as the
 * retry/compact editors in `LLMConfigFields`.
 */
export default function HookPoliciesFields({ task, onChange }: HookPoliciesFieldsProps) {
  const { t } = useTranslation();
  const config: ExecuteConfig = task.execute_config || {};
  const policies: Record<string, Record<string, string>> = config.hook_policies || {};

  const writePolicies = (next: Record<string, Record<string, string>>) => {
    // Drop hook entries that have NO fields at all — keeps JSON tidy and
    // hides `hook_policies: {}` for tasks that don't need any. Transient
    // partially-filled rows (empty key OR empty value) survive so editing
    // doesn't fight the user; matches the HookFields args pattern.
    const cleaned: Record<string, Record<string, string>> = {};
    for (const [hook, fields] of Object.entries(next)) {
      if (Object.keys(fields).length === 0) continue;
      cleaned[hook] = fields;
    }
    onChange({
      execute_config: {
        ...config,
        hook_policies: Object.keys(cleaned).length > 0 ? cleaned : undefined,
      },
    });
  };

  const updateHookField = (hook: string, key: string, value: string) => {
    const nextHook = { ...(policies[hook] || {}) };
    if (key === '' && value === '') {
      // Empty-key, empty-value pair is the "Add field" placeholder — keep it
      // visible so the user can type into it. Same trick as HookFields.addArg.
      nextHook[''] = '';
    } else {
      nextHook[key] = value;
    }
    writePolicies({ ...policies, [hook]: nextHook });
  };

  const renameHookKey = (hook: string, oldKey: string, newKey: string) => {
    const nextHook = { ...(policies[hook] || {}) };
    const value = nextHook[oldKey] ?? '';
    delete nextHook[oldKey];
    if (newKey.trim() !== '') {
      nextHook[newKey] = value;
    }
    writePolicies({ ...policies, [hook]: nextHook });
  };

  const removeHookKey = (hook: string, key: string) => {
    const nextHook = { ...(policies[hook] || {}) };
    delete nextHook[key];
    writePolicies({ ...policies, [hook]: nextHook });
  };

  const addEmptyHook = () => {
    const name = window.prompt(
      t('chains.task_form.hook_policies_add_prompt', 'Hook name (e.g. my_custom_hook):'),
      '',
    );
    const trimmed = (name ?? '').trim();
    if (!trimmed) return;
    if (policies[trimmed]) return;
    // Add a placeholder entry so the new section appears immediately; cleaned
    // out on next write if user doesn't fill anything in.
    writePolicies({ ...policies, [trimmed]: { '': '' } });
  };

  const localShell = policies['local_shell'] || {};
  const localFs = policies['local_fs'] || {};
  const otherHookNames = Object.keys(policies).filter(
    (name) => name !== 'local_shell' && name !== 'local_fs',
  );

  return (
    <Collapsible
      title={t('chains.task_form.hook_policies', 'Hook Policies')}
      defaultExpanded={false}
      className="mt-2">
      <div className="space-y-3 pt-2">
        <P className="text-text-muted text-xs">
          {t(
            'chains.task_form.hook_policies_help',
            'Per-hook allow/deny lists and limits. Hooks with NO policy here may be denied at runtime — local_shell in particular requires _allowed_commands.',
          )}
        </P>

        {/* local_shell */}
        <Collapsible
          title={
            <span className="inline-flex items-center gap-1.5 text-xs">
              {t('chains.task_form.hook_policy_local_shell', 'local_shell')}
              {Object.keys(localShell).length === 0 && (
                <Span variant="muted" className="text-[10px]">
                  · {t('chains.task_form.hook_policy_unset', 'unset')}
                </Span>
              )}
            </span>
          }
          defaultExpanded={Object.keys(localShell).length > 0}
          className="border-border bg-surface-100 dark:bg-dark-surface-200 rounded border">
          <div className="space-y-3 px-3 py-2">
            <div>
              <Label className="block text-sm font-medium">
                {t('chains.task_form.hook_policy_shell_allowed_commands', '_allowed_commands')}
              </Label>
              <Input
                value={localShell['_allowed_commands'] || ''}
                onChange={(e) =>
                  updateHookField('local_shell', '_allowed_commands', e.target.value)
                }
                placeholder="ls,cat,echo,pwd,grep,git,go,python3,node,npm,make,curl,jq"
              />
              <P className="text-text-muted mt-1 text-xs">
                {t(
                  'chains.task_form.hook_policy_shell_allowed_commands_help',
                  'Comma-separated whitelist of shell commands the model may execute. Without this, local_shell denies every call.',
                )}
              </P>
            </div>
            <div>
              <Label className="block text-sm font-medium">
                {t('chains.task_form.hook_policy_shell_denied_commands', '_denied_commands')}
              </Label>
              <Input
                value={localShell['_denied_commands'] || ''}
                onChange={(e) =>
                  updateHookField('local_shell', '_denied_commands', e.target.value)
                }
                placeholder="sudo,su,dd,mkfs,fdisk,parted,shred,rm"
              />
              <P className="text-text-muted mt-1 text-xs">
                {t(
                  'chains.task_form.hook_policy_shell_denied_commands_help',
                  'Comma-separated blacklist applied AFTER the whitelist. Use for explicitly dangerous commands.',
                )}
              </P>
            </div>
          </div>
        </Collapsible>

        {/* local_fs */}
        <Collapsible
          title={
            <span className="inline-flex items-center gap-1.5 text-xs">
              {t('chains.task_form.hook_policy_local_fs', 'local_fs')}
              {Object.keys(localFs).length === 0 && (
                <Span variant="muted" className="text-[10px]">
                  · {t('chains.task_form.hook_policy_unset', 'unset')}
                </Span>
              )}
            </span>
          }
          defaultExpanded={Object.keys(localFs).length > 0}
          className="border-border bg-surface-100 dark:bg-dark-surface-200 rounded border">
          <div className="space-y-3 px-3 py-2">
            <div className="grid grid-cols-2 gap-3">
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.hook_policy_fs_allowed_dir', '_allowed_dir')}
                </Label>
                <Input
                  value={localFs['_allowed_dir'] || ''}
                  onChange={(e) => updateHookField('local_fs', '_allowed_dir', e.target.value)}
                  placeholder="."
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t(
                    'chains.task_form.hook_policy_fs_allowed_dir_help',
                    'Root directory the model can read under (relative to cwd).',
                  )}
                </P>
              </div>
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.hook_policy_fs_denied_paths', '_denied_path_substrings')}
                </Label>
                <Input
                  value={localFs['_denied_path_substrings'] || ''}
                  onChange={(e) =>
                    updateHookField('local_fs', '_denied_path_substrings', e.target.value)
                  }
                  placeholder="node_modules,.git/,dist/,/.next/,/out/,package-lock.json"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t(
                    'chains.task_form.hook_policy_fs_denied_paths_help',
                    'Comma-separated substrings; any path containing one is denied.',
                  )}
                </P>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.hook_policy_fs_max_read_bytes', '_max_read_bytes')}
                </Label>
                <Input
                  value={localFs['_max_read_bytes'] || ''}
                  onChange={(e) =>
                    updateHookField('local_fs', '_max_read_bytes', e.target.value)
                  }
                  placeholder="1048576"
                />
              </div>
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.hook_policy_fs_max_output_bytes', '_max_output_bytes')}
                </Label>
                <Input
                  value={localFs['_max_output_bytes'] || ''}
                  onChange={(e) =>
                    updateHookField('local_fs', '_max_output_bytes', e.target.value)
                  }
                  placeholder="524288"
                />
              </div>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.hook_policy_fs_max_list_depth', '_max_list_depth')}
                </Label>
                <Input
                  value={localFs['_max_list_depth'] || ''}
                  onChange={(e) =>
                    updateHookField('local_fs', '_max_list_depth', e.target.value)
                  }
                  placeholder="6"
                />
              </div>
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.hook_policy_fs_max_grep_matches', '_max_grep_matches')}
                </Label>
                <Input
                  value={localFs['_max_grep_matches'] || ''}
                  onChange={(e) =>
                    updateHookField('local_fs', '_max_grep_matches', e.target.value)
                  }
                  placeholder="5000"
                />
              </div>
            </div>
          </div>
        </Collapsible>

        {/* Other hooks — generic key-value editor per hook name. */}
        {otherHookNames.map((hookName) => {
          const entries = Object.entries(policies[hookName] || {});
          return (
            <Collapsible
              key={hookName}
              title={
                <span className="font-mono text-xs">
                  {hookName}
                  <Span variant="muted" className="ml-1.5 text-[10px]">
                    · {t('chains.task_form.hook_policy_custom', 'custom')}
                  </Span>
                </span>
              }
              defaultExpanded
              className="border-border bg-surface-100 dark:bg-dark-surface-200 rounded border">
              <div className="space-y-2 px-3 py-2">
                {entries.length === 0 ? (
                  <Span variant="muted" className="text-xs">
                    {t(
                      'chains.task_form.hook_policy_empty',
                      'No fields. Click "Add field" below.',
                    )}
                  </Span>
                ) : (
                  entries.map(([key, value]) => (
                    <div key={`${hookName}:${key}`} className="grid grid-cols-[1fr_2fr_auto] gap-2">
                      <Input
                        value={key}
                        onChange={(e) => renameHookKey(hookName, key, e.target.value)}
                        placeholder="_policy_key"
                      />
                      <Input
                        value={value}
                        onChange={(e) => updateHookField(hookName, key, e.target.value)}
                        placeholder="value"
                      />
                      <Button
                        type="button"
                        variant="ghost"
                        size="xs"
                        onClick={() => removeHookKey(hookName, key)}
                        title={t('common.remove', 'Remove')}
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </Button>
                    </div>
                  ))
                )}
                <Button
                  type="button"
                  variant="ghost"
                  size="xs"
                  onClick={() => updateHookField(hookName, '', '')}
                >
                  <Plus className="mr-1 h-3 w-3" />
                  {t('chains.task_form.hook_policy_add_field', 'Add field')}
                </Button>
              </div>
            </Collapsible>
          );
        })}

        <div>
          <Button type="button" variant="ghost" size="xs" onClick={addEmptyHook}>
            <Plus className="mr-1 h-3 w-3" />
            {t('chains.task_form.hook_policy_add_hook', 'Add hook policy')}
          </Button>
        </div>
      </div>
    </Collapsible>
  );
}
