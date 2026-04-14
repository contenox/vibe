import {
  Button,
  ChatMessage as ChatMessageUI,
  ChatStreamThinkingBox,
  ChatStreamingCaret,
  ChatTranscriptStreamingPlaceholder,
  chatTranscriptMarkdownComponents,
} from '@contenox/ui';
import { t } from 'i18next';
import React from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { ChatMessage as ChatMessageModel } from '../../../../lib/types';
import { InlineAttachments } from './InlineAttachmentRenderer';

type ChatMessageProps = {
  message: ChatMessageModel;
  isLatest?: boolean;
  /** Task-event reasoning stream, shown only for the live assistant row. */
  streamThinking?: string;
};

export const ChatMessage = ({ message, isLatest = false, streamThinking }: ChatMessageProps) => {
  const isUser = message.role === 'user';
  const isSystem = message.role === 'system';
  const isTool = message.role === 'tool';

  const roleLabel =
    isUser
      ? t('chat.role_user')
      : isSystem
        ? t('chat.role_system')
        : isTool
          ? t('chat.role_tool', 'Tool')
          : t('chat.role_assistant');

  return (
    <ChatMessageUI
      appearance="transcript"
      role={message.role}
      roleLabel={roleLabel}
      timestamp={new Date(message.sentAt).toLocaleTimeString()}
      timestampTooltip={new Date(message.sentAt).toLocaleString()}
      isLatest={isLatest}
      latestLabel={isLatest ? t('chat.latest') : undefined}
      defaultOpen={!(isSystem || isTool)}
      copyText={message.content}
      copyLabel={t('chat.copy')}
      copiedLabel={t('chat.copied', 'Copied!')}
      collapseToggleLabel={{
        open: t('chat.collapse', 'Hide'),
        closed: t('chat.expand', 'Show'),
      }}
      error={message.error}
      secondaryActions={
        <Button variant="ghost" size="sm" className="h-6 text-xs" type="button">
          {t('chat.share')}
        </Button>
      }
    >
      {message.streaming && streamThinking ? (
        <ChatStreamThinkingBox>{streamThinking}</ChatStreamThinkingBox>
      ) : null}
      {message.streaming && !message.content && !message.error && !streamThinking ? (
        <ChatTranscriptStreamingPlaceholder>
          {t('chat.streaming_placeholder')}
        </ChatTranscriptStreamingPlaceholder>
      ) : null}
      {message.content ? (
        <ReactMarkdown remarkPlugins={[remarkGfm]} components={chatTranscriptMarkdownComponents}>
          {message.content}
        </ReactMarkdown>
      ) : null}
      {message.streaming && message.content && !message.error ? <ChatStreamingCaret /> : null}
      <InlineAttachments attachments={message.attachments} />
    </ChatMessageUI>
  );
};
