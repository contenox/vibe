import { GridLayout, H1, P, Page, Section, Span } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { adminNavItems } from '../../../config/routes';
import { cn } from '../../../lib/utils';

export default function ControlPlanePage() {
  const { t } = useTranslation();

  return (
    <Page bodyScroll="auto">
      <GridLayout variant="body" className="gap-8 pb-8">
        <Section>
          <H1 variant="page">{t('control_plane.hub_title')}</H1>
          <P variant="muted" className="mt-2">
            {t('control_plane.hub_description')}
          </P>
          <nav className="mt-8 grid gap-2 sm:grid-cols-2" aria-label={t('control_plane.hub_title')}>
            {adminNavItems.map(item => (
              <Link
                key={item.path}
                to={item.path}
                className={cn(
                  'border-surface-200 dark:border-dark-surface-600 flex items-center gap-3 rounded-lg border px-4 py-3',
                  'hover:bg-surface-100 dark:hover:bg-dark-surface-100 transition-colors',
                )}>
                {item.icon && (
                  <Span className="text-primary-500 dark:text-dark-primary-500 flex h-8 w-8 shrink-0 items-center justify-center">
                    {item.icon}
                  </Span>
                )}
                <Span className="font-medium">{item.label}</Span>
              </Link>
            ))}
          </nav>
        </Section>
      </GridLayout>
    </Page>
  );
}
