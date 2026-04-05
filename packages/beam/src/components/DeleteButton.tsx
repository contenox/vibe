import { Button, Spinner } from '@contenox/ui';

type Props = {
  label: string;
  confirmMessage: string;
  onConfirm: () => void;
  isPending?: boolean;
  className?: string;
};

export default function ConfirmDeleteButton({
  label,
  confirmMessage,
  onConfirm,
  isPending,
  className,
}: Props) {
  return (
    <Button
      size="sm"
      variant="ghost"
      className={`text-error hover:bg-error/10 ${className || ''}`}
      onClick={() => {
        if (confirm(confirmMessage)) onConfirm();
      }}
      disabled={isPending}>
      {isPending ? <Spinner size="sm" /> : label}
    </Button>
  );
}
