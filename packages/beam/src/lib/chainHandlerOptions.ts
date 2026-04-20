import type { TaskHandler } from './types';

export const CHAIN_HANDLER_OPTIONS: Array<{
  value: TaskHandler;
  label: string;
  hint?: string;
}> = [
  {
    value: 'chat_completion',
    label: 'Chat Completion',
    hint: 'Run full chat (tools, etc.)',
  },
  {
    value: 'execute_tool_calls',
    label: 'Execute Tool Calls',
    hint: 'Run tools that previous model call asked for',
  },
  {
    value: 'hook',
    label: 'Hook',
    hint: 'Call registered remote hook',
  },
  {
    value: 'prompt_to_string',
    label: 'Prompt to String',
    hint: 'Plain LLM answer',
  },
  {
    value: 'prompt_to_int',
    label: 'Prompt to Integer',
    hint: 'Ask LLM for a number, store as int',
  },
  {
    value: 'raise_error',
    label: 'Raise Error',
    hint: 'Fail the chain with the task input as message',
  },
  {
    value: 'noop',
    label: 'No Operation',
    hint: 'Pass input through unchanged',
  },
];
