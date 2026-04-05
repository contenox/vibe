import { useMutation, UseMutationResult, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import { execKeys } from '../lib/queryKeys';
import { formatTaskOutput } from '../lib/taskEvents';
import { ChainDefinition, Exec, ExecResp } from '../lib/types';

type ExecPromptVariables = Exec & {
  requestId?: string;
  signal?: AbortSignal;
};

const promptExecChain: ChainDefinition = {
  id: 'beam-exec-prompt',
  description: 'One-shot prompt execution for Beam',
  tasks: [
    {
      id: 'beam_exec_prompt',
      description: 'Return a single assistant response for the provided prompt.',
      handler: 'prompt_to_string',
      prompt_template: '{{.input}}',
      transition: {
        on_failure: '',
        branches: [{ operator: 'default', when: '', goto: 'end' }],
      },
    },
  ],
};

export function useExecPrompt(): UseMutationResult<ExecResp, Error, ExecPromptVariables, unknown> {
  const queryClient = useQueryClient();
  return useMutation<ExecResp, Error, ExecPromptVariables>({
    mutationFn: async ({ prompt, requestId, signal }) => {
      const response = await api.executeTaskChain(
        {
          input: prompt,
          inputType: 'string',
          chain: promptExecChain,
        },
        { requestId, signal },
      );
      return {
        id: requestId ?? '',
        response: formatTaskOutput(response.output),
      };
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: execKeys.all });
    },
  });
}
