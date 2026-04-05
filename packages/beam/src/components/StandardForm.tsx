import { Button, Form, Spinner } from '@contenox/ui';

interface StandardFormProps {
  title: string;
  onSubmit: (e: React.FormEvent) => void;
  isPending: boolean;
  error?: string;
  onCancel?: () => void;
  submitLabel?: string;
  cancelLabel?: string;
  children: React.ReactNode;
  actions?: React.ReactNode;
}

export default function StandardForm({
  title,
  onSubmit,
  isPending,
  error,
  onCancel,
  submitLabel = 'Save',
  cancelLabel = 'Cancel',
  children,
  actions,
}: StandardFormProps) {
  return (
    <Form
      title={title}
      onSubmit={onSubmit}
      error={error}
      actions={
        actions || (
          <div className="flex gap-2">
            <Button type="submit" variant="primary" disabled={isPending} className="min-w-20">
              {isPending ? (
                <>
                  <Spinner size="sm" className="mr-2" />
                  Saving...
                </>
              ) : (
                submitLabel
              )}
            </Button>
            {onCancel && (
              <Button type="button" variant="secondary" onClick={onCancel} disabled={isPending}>
                {cancelLabel}
              </Button>
            )}
          </div>
        )
      }>
      {children}
    </Form>
  );
}
