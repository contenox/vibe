import { Button, ButtonGroup, DiffView, Panel } from '@contenox/ui';
import type { DiffLine } from '@contenox/ui';
import { useState } from 'react';
import type { PendingApproval } from '../../../../lib/taskEvents';

type Props = {
  approval: PendingApproval;
  onRespond: (approved: boolean) => void;
};

function parsePatch(raw: string): { filePath: string; lines: DiffLine[] } {
  const rawLines = raw.split('\n');
  let filePath = 'diff';
  const lines: DiffLine[] = [];
  let oldLine = 0;
  let newLine = 0;

  for (const text of rawLines) {
    if (text.startsWith('+++ ')) {
      filePath = text.slice(4).replace(/^b\//, '');
      continue;
    }
    if (text.startsWith('--- ')) continue;
    if (text.startsWith('@@ ')) {
      const m = text.match(/@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
      if (m) {
        oldLine = parseInt(m[1], 10);
        newLine = parseInt(m[2], 10);
      }
      lines.push({ type: 'context', content: text });
      continue;
    }
    if (text.startsWith('+')) {
      lines.push({ type: 'add', content: text.slice(1), newLineNumber: newLine++ });
    } else if (text.startsWith('-')) {
      lines.push({ type: 'remove', content: text.slice(1), oldLineNumber: oldLine++ });
    } else {
      lines.push({
        type: 'context',
        content: text.startsWith(' ') ? text.slice(1) : text,
        oldLineNumber: oldLine++,
        newLineNumber: newLine++,
      });
    }
  }

  return { filePath, lines };
}

export function ApprovalCard({ approval, onRespond }: Props) {
  const [inflight, setInflight] = useState(false);
  const [diffExpanded, setDiffExpanded] = useState(false);

  const handle = (approved: boolean) => {
    if (inflight) return;
    setInflight(true);
    onRespond(approved);
  };

  const argEntries = Object.entries(approval.args).filter(
    ([, v]) => v !== '' && v !== null && v !== undefined,
  );

  return (
    <Panel variant="warning" className="mx-0 my-2">
      <div className="mb-1.5 flex items-center gap-1 text-sm font-semibold">
        ⚠ Approval required:{' '}
        <code className="text-[0.9em]">
          {approval.hookName}.{approval.toolName}
        </code>
      </div>

      {argEntries.length > 0 && (
        <table className="mb-2 border-collapse text-xs">
          <tbody>
            {argEntries.map(([k, v]) => (
              <tr key={k}>
                <td className="pr-3 align-top text-text-muted dark:text-dark-text-muted">{k}</td>
                <td className="break-all font-mono">{String(v)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {approval.diff && approval.diff !== '(no changes)' && (
        <div className="mb-2">
          <Button
            variant="ghost"
            size="sm"
            className="px-0 text-text-muted dark:text-dark-text-muted"
            onClick={() => setDiffExpanded(e => !e)}
          >
            {diffExpanded ? '▾ Hide diff' : '▸ Show diff'}
          </Button>
          {diffExpanded && (() => {
            const { filePath, lines } = parsePatch(approval.diff!);
            return <DiffView filePath={filePath} lines={lines} className="max-h-80 overflow-auto" />;
          })()}
        </div>
      )}

      <ButtonGroup className="mt-1">
        <Button size="sm" variant="success" disabled={inflight} onClick={() => handle(true)}>
          Approve
        </Button>
        <Button size="sm" variant="danger" disabled={inflight} onClick={() => handle(false)}>
          Deny
        </Button>
      </ButtonGroup>
    </Panel>
  );
}
