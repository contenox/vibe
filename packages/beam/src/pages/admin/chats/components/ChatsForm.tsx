import { Button, Form } from '@contenox/ui';
import { t } from 'i18next';

type ChatsFormProps = {
  onSubmit: (e: React.FormEvent) => void;
  isPending: boolean;
  error?: string;
  selectedModel: string;
  setSelectedModel: (value: string) => void;
};

export function ChatsForm({ onSubmit, isPending, error }: ChatsFormProps) {
  return (
    <Form
      onSubmit={onSubmit}
      title={t('chat.start_new_chat')}
      error={error}
      onError={errorMsg => console.error('Form error:', errorMsg)}
      actions={
        <>
          <Button type="submit" variant="primary" disabled={isPending}>
            {isPending ? t('common.creating') : t('chat.create_chat')}
          </Button>
        </>
      }>
      <></>
    </Form>
  );
}
