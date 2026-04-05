import { Button, ButtonGroup, P, Section, Spinner } from '@contenox/ui';

interface ResourceCardProps {
  title: string;
  subtitle?: string;
  status?: 'default' | 'success' | 'error' | 'warning';
  children: React.ReactNode;
  actions?: {
    edit?: () => void;
    delete?: () => void;
    custom?: React.ReactNode;
  };
  isLoading?: boolean;
  className?: string;
}

export function ResourceCard({
  title,
  subtitle,
  status = 'default',
  children,
  actions,
  isLoading = false,
  className = '',
}: ResourceCardProps) {
  const statusVariants = {
    default: 'border-l-4 border-l-border',
    success: 'border-l-4 border-l-success',
    error: 'border-l-4 border-l-error',
    warning: 'border-l-4 border-l-warning',
  };

  return (
    <Section
      title={title}
      className={`bg-surface relative rounded-lg ${statusVariants[status]} ${className}`}>
      {subtitle && (
        <P variant="muted" className="mb-3 text-sm">
          {subtitle}
        </P>
      )}

      <div className="space-y-4">{children}</div>

      {actions && (
        <div className="mt-4 border-t pt-4">
          <ButtonGroup className="flex items-center justify-between">
            <div className="flex gap-2">
              {actions.edit && (
                <Button variant="outline" size="sm" onClick={actions.edit} disabled={isLoading}>
                  Edit
                </Button>
              )}
              {actions.custom}
            </div>

            {actions.delete && (
              <Button
                variant="ghost"
                size="sm"
                onClick={actions.delete}
                disabled={isLoading}
                className="text-error hover:bg-error/10">
                {isLoading ? (
                  <>
                    <Spinner size="sm" className="mr-2" />
                    Deleting
                  </>
                ) : (
                  'Delete'
                )}
              </Button>
            )}
          </ButtonGroup>
        </div>
      )}
    </Section>
  );
}
