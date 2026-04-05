import { Button, Card, Panel, SidebarToggle, Span, UserMenu } from '@contenox/ui';
import React, { useContext, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import logoUrl from '../assets/logo.png';
import { useLogout } from '../hooks/useLogout';
import { AuthContext } from '../lib/authContext';
import { cn } from '../lib/utils';
import { SetupWizard } from './setup/SetupWizard';
import { DropdownMenu } from './DropdownMenu';
import { Sidebar } from './sidebar/Sidebar';

type MenuItem = { path: string; label: string; icon?: React.ReactNode };
type Routes = { nav: MenuItem[]; shelf: MenuItem[] };

type Props = {
  routes: Routes;
  defaultOpen?: boolean;
  mainContent: React.ReactNode;
  className?: string;
};

export function Layout({
  routes: { nav, shelf },
  defaultOpen = true,
  mainContent,
  className,
}: Props) {
  const [isSidebarOpen, setSidebarIsOpen] = useState(defaultOpen);
  const [isNavOpen, setNavIsOpen] = useState(false);
  const [isUserMenuOpen, setIsUserMenuOpen] = useState(false);
  const navigate = useNavigate();
  const { user } = useContext(AuthContext);
  const { mutate: logout } = useLogout();
  const location = useLocation();
  const isOnLoginPage = location.pathname === '/login';
  const sidebarDisabled = !user;

  return (
    <div className={cn('bg-background flex h-screen flex-col text-inherit', className)}>
      {/* Top bar (fixed height) */}
      <Panel
        variant="bordered"
        className="flex h-16 shrink-0 items-center justify-between gap-4 bg-inherit px-4 text-inherit">
        <div className="flex items-center gap-4">
          {!sidebarDisabled ? (
            <SidebarToggle
              isOpen={isSidebarOpen}
              onToggle={() => setSidebarIsOpen(!isSidebarOpen)}
            />
          ) : (
            <div className="w-9" />
          )}
          <div className="flex items-center gap-2">
            <div className="flex items-center gap-2">
              <img src={logoUrl} alt="Contenox" className="h-6 w-auto rounded-md" />
              <Span className="hidden text-lg font-semibold sm:block">Beam</Span>
            </div>
          </div>
        </div>

        <div className="flex items-center gap-2">
          {user ? (
            <UserMenu
              isOpen={isUserMenuOpen}
              friendlyName={user.friendlyName}
              mail={user.email}
              logout={logout}
              onToggle={setIsUserMenuOpen}
            />
          ) : (
            !isOnLoginPage && (
              <Button onClick={() => navigate('/login')} variant="primary" size="sm">
                Login
              </Button>
            )
          )}
          {shelf.length > 0 ? (
            <DropdownMenu isOpen={isNavOpen} setIsOpen={setNavIsOpen} items={shelf} />
          ) : null}
        </div>
      </Panel>

      {user ? <SetupWizard /> : null}

      {/* Main row (sidebar + page) */}
      <div className="flex h-full min-h-0 flex-1 overflow-hidden">
        {/* Sidebar column (scrolls independently) */}
        <Sidebar
          disabled={sidebarDisabled}
          isOpen={isSidebarOpen}
          setIsOpen={setSidebarIsOpen}
          items={nav}
        />

        <Card className="min-h-0 min-w-0 flex-1 overflow-hidden bg-inherit">{mainContent}</Card>
      </div>
    </div>
  );
}
