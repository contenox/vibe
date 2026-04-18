import { Button } from '@contenox/ui';
import React, { useState } from 'react';
import type { PendingApproval } from '../../../../lib/taskEvents';

type Props = {
  approval: PendingApproval;
  onRespond: (approved: boolean) => void;
};

/**
 * ApprovalCard renders a HITL (human-in-the-loop) approval request inline
 * in the chat-canvas. It shows the hook/tool name, arguments, and a unified
 * diff (if the tool mutates a file), then lets the user approve or deny.
 *
 * Execution is paused on the backend until the user responds.
 */
export function ApprovalCard({ approval, onRespond }: Props) {
  const [inflight, setInflight] = useState(false);
  const [diffExpanded, setDiffExpanded] = useState(false);

  const handle = (approved: boolean) => {
    if (inflight) return;
    setInflight(true);
    onRespond(approved);
  };

  const argEntries = Object.entries(approval.args).filter(([, v]) => v !== '' && v !== null && v !== undefined);

  return (
    <div
      style={{
        border: '1px solid var(--color-warning, #d97706)',
        borderRadius: '0.5rem',
        padding: '0.75rem 1rem',
        margin: '0.5rem 0',
        background: 'var(--color-warning-bg, #fffbeb)',
      }}
    >
      <div style={{ fontWeight: 600, marginBottom: '0.4rem' }}>
        ⚠ Approval required:{' '}
        <code style={{ fontSize: '0.9em' }}>
          {approval.hookName}.{approval.toolName}
        </code>
      </div>

      {argEntries.length > 0 && (
        <table style={{ fontSize: '0.8em', marginBottom: '0.5rem', borderCollapse: 'collapse' }}>
          <tbody>
            {argEntries.map(([k, v]) => (
              <tr key={k}>
                <td style={{ paddingRight: '0.75rem', color: 'var(--color-muted, #6b7280)', verticalAlign: 'top' }}>
                  {k}
                </td>
                <td style={{ fontFamily: 'monospace', wordBreak: 'break-all' }}>
                  {String(v)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {approval.diff && approval.diff !== '(no changes)' && (
        <div style={{ marginBottom: '0.5rem' }}>
          <button
            onClick={() => setDiffExpanded(e => !e)}
            style={{
              background: 'none',
              border: 'none',
              cursor: 'pointer',
              fontSize: '0.8em',
              color: 'var(--color-muted, #6b7280)',
              padding: 0,
              marginBottom: '0.25rem',
            }}
          >
            {diffExpanded ? '▾ Hide diff' : '▸ Show diff'}
          </button>
          {diffExpanded && (
            <pre
              style={{
                fontSize: '0.75em',
                background: 'var(--color-code-bg, #f3f4f6)',
                padding: '0.5rem',
                borderRadius: '0.25rem',
                overflowX: 'auto',
                whiteSpace: 'pre',
                maxHeight: '20rem',
                overflowY: 'auto',
              }}
            >
              {approval.diff.split('\n').map((line, i) => {
                const color =
                  line.startsWith('+') ? 'var(--color-success, #16a34a)' :
                  line.startsWith('-') ? 'var(--color-error, #dc2626)' :
                  undefined;
                return (
                  <span key={i} style={color ? { color, display: 'block' } : { display: 'block' }}>
                    {line}
                  </span>
                );
              })}
            </pre>
          )}
        </div>
      )}

      <div style={{ display: 'flex', gap: '0.5rem', marginTop: '0.25rem' }}>
        <Button
          size="sm"
          disabled={inflight}
          onClick={() => handle(true)}
          style={{ background: 'var(--color-success, #16a34a)', color: '#fff' }}
        >
          Approve
        </Button>
        <Button
          size="sm"
          variant="destructive"
          disabled={inflight}
          onClick={() => handle(false)}
        >
          Deny
        </Button>
      </div>
    </div>
  );
}
