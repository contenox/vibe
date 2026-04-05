import { GridLayout, TabbedPage } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import FilesSection from './components/FilesSection';

export default function FilesPage() {
  const { t } = useTranslation();

  const tabs = [
    {
      id: 'files',
      label: t('files.tab_files'),
      content: <FilesSection />,
    },
    // {
    //   id: 'keywords',
    //   label: t('files.tab_keywords'),
    //   content: <KeywordsSection />,
    // },
  ];

  return (
    <GridLayout variant="body">
      <TabbedPage tabs={tabs} />
    </GridLayout>
  );
}
