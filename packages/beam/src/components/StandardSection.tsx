import { EmptyState, Panel, Section, Spinner } from '@contenox/ui';
import React from 'react';

type Props = {
  title?: string;
  isLoading?: boolean;
  error?: { message?: string } | null;
  empty?: boolean;
  emptyTitle?: string;
  emptyDescription?: string;
  children?: React.ReactNode;
  className?: string;
};

export default function StandardSection({
  title,
  isLoading,
  error,
  empty,
  emptyTitle,
  emptyDescription,
  children,
  className,
}: Props) {
  return (
    <Section title={title} className={className}>
      {isLoading ? (
        <div className="flex items-center justify-center py-8">
          <Spinner />
        </div>
      ) : error ? (
        <Panel variant="error">{error.message || 'Something went wrong'}</Panel>
      ) : empty ? (
        <EmptyState title={emptyTitle || 'No data'} description={emptyDescription || ''} />
      ) : (
        children
      )}
    </Section>
  );
}
