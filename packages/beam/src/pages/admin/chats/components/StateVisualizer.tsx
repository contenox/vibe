import { Table, TableCell, TableRow } from '@contenox/ui';
import { t } from 'i18next';
import { CapturedStateUnit } from '../../../../lib/types';

interface StateVisualizerProps {
  state: CapturedStateUnit[];
}

export const StateVisualizer = ({ state }: StateVisualizerProps) => {
  if (!state || state.length === 0) {
    return null;
  }

  const formatDuration = (ms: number): string => {
    if (ms < 1000) return `${Math.round(ms)} ms`;
    return `${(ms / 1000).toFixed(2)} s`;
  };

  return (
    <Table
      columns={[
        t('chat.task_id'),
        t('chat.task_type'),
        t('chat.input_type'),
        t('chat.output_type'),
        t('chat.transition'),
        t('chat.duration'),
        t('chat.error'),
      ]}>
      {state.map((unit, index) => (
        <TableRow key={index} className={unit.error ? 'bg-error/10' : ''}>
          <TableCell>{unit.taskID}</TableCell>
          <TableCell>{unit.taskType}</TableCell>
          <TableCell>{unit.inputType}</TableCell>
          <TableCell>{unit.outputType}</TableCell>
          <TableCell className="max-w-xs truncate">{unit.transition || '-'}</TableCell>
          <TableCell>{formatDuration(unit.duration)}</TableCell>
          <TableCell>
            {unit.error ? <span className="text-error text-sm">{unit.error.error}</span> : '-'}
          </TableCell>
        </TableRow>
      ))}
    </Table>
  );
};
