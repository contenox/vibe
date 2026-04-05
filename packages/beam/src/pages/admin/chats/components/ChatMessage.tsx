import { Button, ChatMessage as ChatMessageUI } from '@contenox/ui';
import { t } from 'i18next';
import React, { type Components } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { ChatMessage as ChatMessageModel } from '../../../../lib/types';

type ChatMessageProps = {
  message: ChatMessageModel;
  isLatest?: boolean;
};

export const ChatMessage = ({ message, isLatest = false }: ChatMessageProps) => {
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
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={markdownComponents}>
        {message.content}
      </ReactMarkdown>
    </ChatMessageUI>
  );
};

const markdownComponents: Components = {
  code: (props => {
    const { inline, className, children, ...rest } = props as {
      inline?: boolean;
      className?: string;
      children?: React.ReactNode;
      [key: string]: unknown;
    };
    const isInline = inline ?? false;

    if (!isInline) {
      return (
        <pre className="bg-surface-200 text-text dark:bg-dark-surface-700 dark:text-dark-text overflow-auto rounded-lg p-4 text-sm">
          <code className={className} {...rest}>
            {children}
          </code>
        </pre>
      );
    }

    return (
      <code
        className="bg-surface-200 text-text dark:bg-dark-surface-700 dark:text-dark-text rounded px-1.5 py-0.5 font-mono text-xs"
        {...rest}
      >
        {children}
      </code>
    );
  }) as Components['code'],

  blockquote: ({ children, ...props }) => (
    <blockquote
      className="border-primary-300 dark:border-dark-primary-400 bg-primary-50 text-text dark:bg-dark-surface-600 dark:text-dark-text my-3 rounded-r-lg border-l-4 py-2 pl-4"
      {...props}
    >
      {children}
    </blockquote>
  ),
};
