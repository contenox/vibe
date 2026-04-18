import { Button, Panel, SidebarToggle, Span, Spinner, UserMenu } from '@contenox/ui';
import React, { useContext, useMemo, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import logoUrl from '../assets/logo.svg?url';
import { useLogout } from '../hooks/useLogout';
import { useSetupStatus } from '../hooks/useSetupStatus';
import { AuthContext } from '../lib/authContext';
import { cn } from '../lib/utils';
import { ControlPlaneDropdown } from './ControlPlaneDropdown';
import { OnboardingWizard } from './setup/OnboardingWizard';
import { Sidebar } from './sidebar/Sidebar';

const ONBOARDING_KEY = 'beam_onboarding_dismissed';

function isDismissed(): boolean {
  try {
    return localStorage.getItem(ONBOARDING_KEY) === '1';
  } catch {
    return false;
  }
}

type Props = {
  defaultOpen?: boolean;
  mainContent: React.ReactNode;
  /** Left rail content (e.g. chat sessions). */
  sidebarContent: React.ReactNode;
  className?: string;
};

export function Layout({
  defaultOpen = true,
  mainContent,
  sidebarContent,
  className,
}: Props) {
  const [isSidebarOpen, setSidebarIsOpen] = useState(defaultOpen);
  const [isControlPlaneOpen, setControlPlaneOpen] = useState(false);
  const [isUserMenuOpen, setIsUserMenuOpen] = useState(false);
  const navigate = useNavigate();
  const { user } = useContext(AuthContext);
  const { mutate: logout } = useLogout();
  const location = useLocation();
  const isOnLoginPage = location.pathname === '/login';
  const sidebarDisabled = !user;

  const [wizardDismissed, setWizardDismissed] = useState(isDismissed);
  const { data: setupData, isLoading: setupLoading } = useSetupStatus(!!user);

  const setupComplete = useMemo(() => {
    if (!setupData) return false;
    const hasErrors = (setupData.issues ?? []).some(i => i.severity === 'error');
    return !hasErrors && setupData.reachableBackendCount > 0;
  }, [setupData]);

  const showWizard = !!user && !wizardDismissed && !setupLoading && !setupComplete;

  const dismissWizard = () => {
    try {
      localStorage.setItem(ONBOARDING_KEY, '1');
    } catch {}
    setWizardDismissed(true);
  };

  const navbar = (
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
          <img src={logoUrl} alt="Contenox" className="h-6 w-auto rounded-md" />
          <Span className="hidden text-lg font-semibold sm:block">Beam</Span>
        </div>
      </div>

      <div className="flex items-center gap-2">
        {user ? (
          <>
            <ControlPlaneDropdown isOpen={isControlPlaneOpen} setIsOpen={setControlPlaneOpen} />
            <UserMenu
              isOpen={isUserMenuOpen}
              friendlyName={user.friendlyName}
              mail={user.email}
              logout={logout}
              onToggle={setIsUserMenuOpen}
            />
          </>
        ) : (
          !isOnLoginPage && (
            <Button onClick={() => navigate('/login')} variant="primary" size="sm">
              Login
            </Button>
          )
        )}
      </div>
    </Panel>
  );

  // While waiting for setup status, show a spinner so we never flash the app behind the wizard
  if (user && setupLoading) {
    return (
      <div className={cn('bg-background flex h-screen flex-col text-inherit', className)}>
        {navbar}
        <div className="flex flex-1 items-center justify-center">
          <Spinner size="md" />
        </div>
      </div>
    );
  }

  if (showWizard) {
    return (
      <div className={cn('bg-background flex h-screen flex-col text-inherit', className)}>
        {navbar}
        <div className="flex-1 min-h-0 overflow-hidden">
          <OnboardingWizard data={setupData} onDismiss={dismissWizard} />
        </div>
      </div>
    );
  }

  return (
    <div className={cn('bg-background flex h-screen flex-col text-inherit', className)}>
      {navbar}
      <div className="flex h-full min-h-0 flex-1 overflow-hidden">
        <Sidebar
          disabled={sidebarDisabled}
          isOpen={isSidebarOpen}
          setIsOpen={setSidebarIsOpen}
          items={[]}>
          {sidebarContent}
        </Sidebar>
        <main className="bg-background min-h-0 min-w-0 flex-1 overflow-hidden">{mainContent}</main>
      </div>
    </div>
  );
}
