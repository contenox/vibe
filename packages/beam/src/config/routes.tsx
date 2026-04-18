import {
  ChevronsRight,
  Database,
  File,
  Link as LinkIcon,
  ListChecks,
  Package,
  Settings,
  type LucideIcon,
} from 'lucide-react';
import { lazy } from 'react';
import { Navigate } from 'react-router-dom';
import i18n from '../i18n';
import HomeRedirect from '../pages/HomeRedirect.tsx';
import { LOGIN_ROUTE } from './routeConstants.ts';

const BackendsPage = lazy(() => import('../pages/admin/backends/BackendPage.tsx'));
const ModelRegistryPage = lazy(() => import('../pages/admin/models/ModelRegistryPage.tsx'));
const ChainsPage = lazy(() => import('../pages/admin/chains/ChainsPage.tsx'));
const HITLPoliciesPage = lazy(() => import('../pages/admin/hitl-policies/HITLPoliciesPage.tsx'));
const PlansListPage = lazy(() => import('../pages/admin/plans/PlansListPage.tsx'));
const PlanActivePage = lazy(() => import('../pages/admin/plans/PlanActivePage.tsx'));
const ChatPage = lazy(() => import('../pages/admin/chats/ChatPage.tsx'));
const ChatLandingPage = lazy(() => import('../pages/admin/chats/ChatLandingPage.tsx'));
const ControlPlanePage = lazy(() => import('../pages/admin/control/ControlPlanePage.tsx'));
const FilesPage = lazy(() => import('../pages/admin/files/FilesPage.tsx'));
const ExecPromptPage = lazy(() => import('../pages/admin/prompt/ExecPromptPage.tsx'));
const RemoteHooksPage = lazy(() => import('../pages/admin/remotehooks/RemoteHooksPage.tsx'));
const SettingsPage = lazy(() => import('../pages/admin/settings/SettingsPage.tsx'));
const ByePage = lazy(() => import('../pages/public/bye/Bye.tsx'));
const AuthPage = lazy(() => import('../pages/public/login/AuthPage.tsx'));

const LegacyChatsRedirect = () => <Navigate to="/chat" replace />;

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

export type AdminNavItem = {
  path: string;
  label: string;
  icon?: React.ReactNode;
};

type AdminRouteDefinition = {
  path: string;
  element: React.ComponentType;
  labelKey: string;
  Icon: LucideIcon;
};

const adminRouteDefinitions: AdminRouteDefinition[] = [
  { path: '/backends', element: BackendsPage, labelKey: 'navbar.backends', Icon: Database },
  { path: '/model-registry', element: ModelRegistryPage, labelKey: 'navbar.model_registry', Icon: Package },
  { path: '/hooks/remote', element: RemoteHooksPage, labelKey: 'navbar.hooks', Icon: LinkIcon },
  { path: '/files', element: FilesPage, labelKey: 'navbar.files', Icon: File },
  { path: '/chains', element: ChainsPage, labelKey: 'navbar.chains', Icon: LinkIcon },
  { path: '/hitl-policies', element: HITLPoliciesPage, labelKey: 'navbar.hitl_policies', Icon: Settings },
  { path: '/plans', element: PlansListPage, labelKey: 'navbar.plans', Icon: ListChecks },
  { path: '/exec', element: ExecPromptPage, labelKey: 'navbar.prompt', Icon: ChevronsRight },
  { path: '/settings', element: SettingsPage, labelKey: 'navbar.settings', Icon: Settings },
];

/** Admin destinations for the control-plane menu and hub; route paths unchanged. */
export const adminNavItems: AdminNavItem[] = adminRouteDefinitions.map(
  ({ path, labelKey, Icon }) => ({
    path,
    label: i18n.t(labelKey),
    icon: <Icon className="h-[1em] w-[1em]" />,
  }),
);

const adminRoutes: RouteConfig[] = adminRouteDefinitions.map(def => ({
  path: def.path,
  element: def.element,
  label: i18n.t(def.labelKey),
  icon: <def.Icon className="h-[1em] w-[1em]" />,
  showInNav: false,
  protected: true,
  showInShelf: false,
}));

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
    path: '/chat',
    element: ChatLandingPage,
    label: '',
    showInNav: false,
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
    element: LegacyChatsRedirect,
    label: '',
    showInNav: false,
    protected: true,
    showInShelf: false,
  },
  {
    path: '/control',
    element: ControlPlanePage,
    label: '',
    showInNav: false,
    protected: true,
    showInShelf: false,
  },
  ...adminRoutes,
  {
    path: '/plans/active',
    element: PlanActivePage,
    label: '',
    showInNav: false,
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
