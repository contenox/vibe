// src/components/Page.tsx
import React from 'react';
import { cn } from '../lib/utils';

type PageProps = {
  header?: React.ReactNode; // optional header (filters, tabs, etc.)
  footer?: React.ReactNode; // optional footer (sticky actions)
  children: React.ReactNode; // the page body
  className?: string;
  /* bodyScroll:
     - "auto": common form/list pages (content scrolls)
     - "hidden": canvas/map/editors that manage their own fill via <Fill>
  */
  bodyScroll?: 'auto' | 'hidden';
};

export function Page({ header, footer, children, className, bodyScroll = 'auto' }: PageProps) {
  return (
    <div className={cn('flex h-full min-h-0 flex-col', className)}>
      {header && <div className="shrink-0">{header}</div>}

      <div
        className={cn(
          'flex min-h-0 w-full max-w-full min-w-0 flex-1 flex-col overflow-x-clip',
          bodyScroll === 'auto' ? 'overflow-y-auto' : 'overflow-y-hidden',
        )}>
        {children}
      </div>

      {footer && <div className="shrink-0">{footer}</div>}
    </div>
  );
}

/** A child that tells its content to **fill** the remaining height.
 * Use inside <Page bodyScroll="hidden"> when you render canvases, editors, etc.
 */
export function Fill({ children, className }: { children: React.ReactNode; className?: string }) {
  return <div className={cn('relative min-h-0 min-w-0 flex-1', className)}>{children}</div>;
}
