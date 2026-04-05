import { ChatComposer, Tooltip, Badge } from '@contenox/ui';
import { t } from 'i18next';
import { FormEvent } from 'react';

type MessageInputFormProps = {
  value: string;
  onChange: (value: string) => void;
  onSubmit: (e: FormEvent) => void;
  placeholder?: string;
  isPending: boolean;
  buttonLabel?: string;
  title: string;
  className?: string;
  variant?: 'default' | 'compact';
  canSubmit?: boolean;
};

export const MessageInputForm = ({
  value,
  onChange,
  onSubmit,
  placeholder = t('chat.input_placeholder'),
  isPending,
  buttonLabel = t('chat.send_button'),
  title,
  className,
  variant = 'default',
  canSubmit,
}: MessageInputFormProps) => {
  return (
    <ChatComposer
      value={value}
      onChange={onChange}
      onSubmit={onSubmit}
      placeholder={placeholder}
      isPending={isPending}
      submitLabel={buttonLabel}
      pendingLabel={t('chat.sending_button')}
      title={title}
      className={className}
      variant={variant}
      maxLength={4000}
      canSubmit={canSubmit}
      charCountTooltip={variant === 'default' ? t('chat.char_count_tooltip') : undefined}
      footerStart={
        variant === 'default' ? (
          <>
            <Tooltip content={t('chat.enter_to_send')}>
              <Badge variant="outline" size="sm">
                ⏎ {t('chat.send')}
              </Badge>
            </Tooltip>
            <Tooltip content={t('chat.shift_enter_newline')}>
              <Badge variant="outline" size="sm">
                ⇧ + ⏎ {t('chat.new_line')}
              </Badge>
            </Tooltip>
          </>
        ) : undefined
      }
    />
  );
};
