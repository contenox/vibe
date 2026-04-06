import type { ChatMessage } from '../../../lib/types';

export type ChatThreadItem =
  | { kind: 'message'; message: ChatMessage }
  | { kind: 'compiledPlanEmbed'; key: string };

/**
 * Inserts a compiled-plan embed after all persisted messages and before the live streaming row (if any).
 */
export function buildChatThreadItems(options: {
  displayHistory: ChatMessage[];
  insertCompiledPlanEmbed: boolean;
  embedKey: string;
}): ChatThreadItem[] {
  const { displayHistory, insertCompiledPlanEmbed, embedKey } = options;
  if (!insertCompiledPlanEmbed) {
    return displayHistory.map(m => ({ kind: 'message', message: m }));
  }

  const last = displayHistory[displayHistory.length - 1];
  const hasLiveStreaming = Boolean(last?.streaming);

  if (hasLiveStreaming && displayHistory.length > 0) {
    const base = displayHistory.slice(0, -1);
    return [
      ...base.map(m => ({ kind: 'message' as const, message: m })),
      { kind: 'compiledPlanEmbed' as const, key: embedKey },
      { kind: 'message' as const, message: last },
    ];
  }

  return [
    ...displayHistory.map(m => ({ kind: 'message' as const, message: m })),
    { kind: 'compiledPlanEmbed' as const, key: embedKey },
  ];
}
