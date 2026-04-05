import React from 'react';
import { useMe } from '../hooks/useMe';
import { AuthContext } from './authContext';

export const AuthProvider = ({ children }: { children: React.ReactNode }) => {
  const { data: user, isLoading, isError, error } = useMe();

  return (
    <AuthContext.Provider value={{ user, isLoading, isError, error: error as Error | null }}>
      {children}
    </AuthContext.Provider>
  );
};
