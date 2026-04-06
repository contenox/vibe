import { Button } from "./Button";
import { ButtonGroup } from "./ButtonGroup";
import { Section } from "./Section";
import { Spinner } from "./Spinner";
import { P } from "./Typography";

interface ResourceCardProps {
  title: string;
  subtitle?: string;
  status?: "default" | "success" | "error" | "warning";
  children: React.ReactNode;
  actions?: {
    edit?: () => void;
    delete?: () => void;
    custom?: React.ReactNode;
  };
  isLoading?: boolean;
  className?: string;
}

const statusBorderStyles: Record<
  NonNullable<ResourceCardProps["status"]>,
  string
> = {
  default: "border-l-4 border-l-border dark:border-l-dark-surface-600",
  success: "border-l-4 border-l-success dark:border-l-dark-success",
  error: "border-l-4 border-l-error dark:border-l-dark-error",
  warning: "border-l-4 border-l-warning dark:border-l-dark-warning",
};

export function ResourceCard({
  title,
  subtitle,
  status = "default",
  children,
  actions,
  isLoading = false,
  className = "",
}: ResourceCardProps) {
  return (
    <Section
      title={title}
      className={`bg-surface dark:bg-dark-surface-100 relative rounded-lg ${statusBorderStyles[status]} ${className}`}
    >
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
                <Button
                  variant="outline"
                  size="sm"
                  onClick={actions.edit}
                  disabled={isLoading}
                >
                  Edit
                </Button>
              )}
              {actions.custom}
            </div>

            {actions.delete && (
              <Button
                variant="danger"
                size="sm"
                onClick={actions.delete}
                disabled={isLoading}
              >
                {isLoading ? (
                  <>
                    <Spinner size="sm" className="mr-2" />
                    Deleting
                  </>
                ) : (
                  "Delete"
                )}
              </Button>
            )}
          </ButtonGroup>
        </div>
      )}
    </Section>
  );
}
