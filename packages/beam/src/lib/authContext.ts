import { createContext } from 'react';
import { AuthenticatedUser } from './types';

export interface AuthContextType {
  user: AuthenticatedUser | undefined;
  isLoading: boolean;
  isError: boolean;
  error: Error | null;
}

export const AuthContext = createContext<AuthContextType>({
  user: undefined,
  isLoading: true,
  isError: false,
  error: null,
});
