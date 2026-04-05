import { Select } from '@contenox/ui';
import { useTranslation } from 'react-i18next';

interface HandlerSelectorProps {
  value: string;
  onChange: (value: string) => void;
}

export default function HandlerSelector({ value, onChange }: HandlerSelectorProps) {
  const { t } = useTranslation();

  const options = [
    { value: 'prompt_to_string', label: 'Prompt to String' },
    { value: 'chat_completion', label: 'Chat Completion' },
    { value: 'hook', label: 'Hook' },
    { value: 'prompt_to_int', label: 'Prompt to Integer' },
    { value: 'prompt_to_float', label: 'Prompt to Float' },
    { value: 'prompt_to_range', label: 'Prompt to Range' },
    { value: 'prompt_to_condition', label: 'Prompt to Condition' },
    { value: 'text_to_embedding', label: 'Text to Embedding' },
    { value: 'execute_tool_calls', label: 'Execute Tool Calls' },
    { value: 'parse_command', label: 'Parse Command' },
    { value: 'parse_key_value', label: 'Parse Key Value' },
    { value: 'convert_to_openai_chat_response', label: 'Convert to OpenAI Chat Response' },
    { value: 'noop', label: 'No Operation' },
    { value: 'raise_error', label: 'Raise Error' },
  ];

  const handleChange = (e: React.ChangeEvent<HTMLSelectElement>) => {
    onChange(e.target.value);
  };

  return (
    <Select
      value={value}
      options={options}
      onChange={handleChange}
      placeholder={t('chains.task_form.select_handler')}
    />
  );
}
