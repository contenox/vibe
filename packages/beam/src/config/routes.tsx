import { P } from '@contenox/ui';
import {
  ChevronsRight,
  Database,
  File,
  Link,
  ListChecks,
  MessageCircleCode,
  Settings,
} from 'lucide-react';
import { lazy } from 'react';
import i18n from '../i18n';
import HomeRedirect from '../pages/HomeRedirect.tsx';
import { LOGIN_ROUTE } from './routeConstants.ts';

const BackendsPage = lazy(() => import('../pages/admin/backends/BackendPage.tsx'));
const ChainsPage = lazy(() => import('../pages/admin/chains/ChainsPage.tsx'));
const PlansPage = lazy(() => import('../pages/admin/plans/PlansPage.tsx'));
const ChatPage = lazy(() => import('../pages/admin/chats/ChatPage.tsx'));
const ChatsListPage = lazy(() => import('../pages/admin/chats/components/ChatListPage.tsx'));
const FilesPage = lazy(() => import('../pages/admin/files/FilesPage.tsx'));
const ExecPromptPage = lazy(() => import('../pages/admin/prompt/ExecPromptPage.tsx'));
const RemoteHooksPage = lazy(() => import('../pages/admin/remotehooks/RemoteHooksPage.tsx'));
const ByePage = lazy(() => import('../pages/public/bye/Bye.tsx'));
const AuthPage = lazy(() => import('../pages/public/login/AuthPage.tsx'));

interface RouteConfig {
  path: string;
  element: React.ComponentType;
  label: string;
  icon?: React.ReactNode;
  showInNav?: boolean;
  system?: boolean;
  protected: boolean;
  showInShelf?: boolean;
}

export const routes: RouteConfig[] = [
  {
    path: '/',
    element: HomeRedirect,
    label: '',
    showInNav: false,
    protected: false,
    showInShelf: false,
  },
  {
    path: '/backends',
    element: BackendsPage,
    label: i18n.t('navbar.backends'),
    icon: <Database className="h-[1em] w-[1em]" />,
    showInNav: true,
    protected: true,
    showInShelf: false,
  },

  {
    path: '/hooks/remote',
    element: RemoteHooksPage,
    label: i18n.t('navbar.hooks'),
    icon: <Link className="h-[1em] w-[1em]" />,
    showInNav: true,
    protected: true,
    showInShelf: false,
  },
  {
    path: '/files',
    element: FilesPage,
    label: i18n.t('navbar.files'),
    icon: <File className="h-[1em] w-[1em]" />,
    showInNav: true,
    protected: true,
    showInShelf: false,
  },
  {
    path: '/chat/:chatId',
    element: ChatPage,
    label: i18n.t('navbar.chat'),
    showInNav: false,
    protected: true,
    showInShelf: false,
  },
  {
    path: '/chats',
    element: ChatsListPage,
    label: i18n.t('navbar.chats'),
    icon: <MessageCircleCode className="h-[1em] w-[1em]" />,
    showInNav: true,
    protected: true,
    showInShelf: false,
  },
  {
    path: '/chains',
    element: ChainsPage,
    label: i18n.t('navbar.chains'),
    icon: <Link className="h-[1em] w-[1em]" />,
    showInNav: true,
    protected: true,
    showInShelf: false,
  },
  {
    path: '/plans',
    element: PlansPage,
    label: i18n.t('navbar.plans'),
    icon: <ListChecks className="h-[1em] w-[1em]" />,
    showInNav: true,
    protected: true,
    showInShelf: false,
  },
  {
    path: '/exec',
    element: ExecPromptPage,
    label: i18n.t('navbar.prompt'),
    icon: <ChevronsRight className="h-[1em] w-[1em]" />,
    showInNav: true,
    protected: true,
    showInShelf: false,
  },
  {
    path: '/settings',
    element: () => <P>{i18n.t('navbar.settings')}</P>,
    label: i18n.t('navbar.settings'),
    icon: <Settings className="h-[1em] w-[1em]" />,
    showInNav: true,
    protected: true,
    showInShelf: false,
  },
  {
    path: LOGIN_ROUTE,
    element: AuthPage,
    label: i18n.t('login.title'),
    showInNav: false,
    protected: false,
    showInShelf: false,
  },
  {
    path: '/bye',
    element: ByePage,
    label: i18n.t('navbar.bye'),
    showInNav: false,
    system: true,
    protected: false,
    showInShelf: false,
  },
  {
    path: '*',
    element: () => i18n.t('pages.not_found'),
    label: i18n.t('pages.not_found'),
    showInNav: false,
    system: true,
    protected: false,
    showInShelf: false,
  },
];
