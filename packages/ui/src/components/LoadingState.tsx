import { Button } from "./Button";
import { Panel } from "./Panel";
import { P } from "./Typography";
import { Spinner } from "./Spinner";

export function LoadingState({
  message = "Loading...",
}: {
  message?: string;
}) {
  return (
    <div className="flex items-center justify-center py-12">
      <div className="text-center">
        <Spinner size="lg" className="mx-auto mb-4" />
        <P variant="muted">{message}</P>
      </div>
    </div>
  );
}

export function ErrorState({
  error,
  onRetry,
  title = "Error",
  description = "An error occurred while loading data.",
}: {
  error?: Error | string;
  onRetry?: () => void;
  title?: string;
  description?: string;
}) {
  return (
    <Panel variant="error" className="p-6">
      <div className="text-center">
        <P className="mb-2 font-medium">{title}</P>
        <P variant="muted" className="mb-4">
          {typeof error === "string" ? error : error?.message || description}
        </P>
        {onRetry && (
          <Button variant="outline" onClick={onRetry}>
            Try Again
          </Button>
        )}
      </div>
    </Panel>
  );
}
