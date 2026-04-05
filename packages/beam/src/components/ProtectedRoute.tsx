import { Spinner } from '@contenox/ui';
import { JSX, useContext } from 'react';
import { Navigate } from 'react-router-dom';
import { LOGIN_ROUTE } from '../config/routeConstants';
import { AuthContext } from '../lib/authContext';

interface ProtectedRouteProps {
  children: JSX.Element;
}

export function ProtectedRoute({ children }: ProtectedRouteProps): JSX.Element {
  const { user, isLoading, isError } = useContext(AuthContext);
  if (isLoading) {
    return (
      <div className="flex h-screen w-full items-center justify-center">
        <Spinner />
      </div>
    );
  }

  if (isError || !user) {
    return <Navigate to={LOGIN_ROUTE} replace />;
  }
  return children;
}
