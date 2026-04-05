import type { TaskHandler } from './types';

export const CHAIN_HANDLER_OPTIONS: Array<{
  value: TaskHandler;
  label: string;
  hint?: string;
}> = [
  {
    value: 'prompt_to_condition',
    label: 'Prompt to Condition',
    hint: 'LLM returns a key that you map in valid_conditions',
  },
  {
    value: 'prompt_to_int',
    label: 'Prompt to Integer',
    hint: 'Ask LLM for a number, store as int',
  },
  {
    value: 'prompt_to_float',
    label: 'Prompt to Float',
    hint: 'Ask LLM for a float, store as number',
  },
  {
    value: 'prompt_to_range',
    label: 'Prompt to Range',
    hint: `e.g. "6-8" or "6"`,
  },
  {
    value: 'prompt_to_string',
    label: 'Prompt to String',
    hint: 'Plain LLM answer',
  },
  {
    value: 'text_to_embedding',
    label: 'Text to Embedding',
    hint: 'Turn text into vector',
  },
  {
    value: 'raise_error',
    label: 'Raise Error',
    hint: 'Fail the chain with the task input as message',
  },
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
    value: 'parse_command',
    label: 'Parse Command',
    hint: 'Parse /command style outputs to branch',
  },
  {
    value: 'parse_key_value',
    label: 'Parse Key/Value',
    hint: 'Parse "a=1, b=2" into JSON',
  },
  {
    value: 'convert_to_openai_chat_response',
    label: 'Convert to OpenAI Chat Response',
    hint: 'Wrap chat_history into OpenAI response shape',
  },
  {
    value: 'noop',
    label: 'No Operation',
    hint: 'Pass input through unchanged',
  },
  {
    value: 'hook',
    label: 'Hook',
    hint: 'Call registered remote hook',
  },
];
