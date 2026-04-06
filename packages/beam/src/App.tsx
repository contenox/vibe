import { Spinner } from '@contenox/ui';
import '@contenox/ui/styles.css';
import { Suspense } from 'react';
import { Route, BrowserRouter as Router, Routes } from 'react-router-dom';
import './app.css';
import { ErrorBoundary } from './components/ErrorBoundary';
import { Layout } from './components/Layout';
import { ProtectedRoute } from './components/ProtectedRoute';
import { ChatSessionSidebar } from './components/sidebar/ChatSessionSidebar';
import { routes } from './config/routes';
import { AuthProvider } from './lib/AuthProvider';

export default function App() {
  return (
    <Router>
      <AuthProvider>
        <Layout
          sidebarContent={<ChatSessionSidebar />}
          defaultOpen={true}
          mainContent={
            <ErrorBoundary>
              <Suspense fallback={<div className="flex min-h-screen items-center justify-center"><Spinner /></div>}>
                <Routes>
                  {routes.map((route, index) => {
                    const Element = route.element;
                    const wrappedElement =
                      route.protected !== false ? (
                        <ProtectedRoute>
                          <Element />
                        </ProtectedRoute>
                      ) : (
                        <Element />
                      );
                    return <Route key={index} path={route.path} element={wrappedElement} />;
                  })}
                </Routes>
              </Suspense>
            </ErrorBoundary>
          }
        />
      </AuthProvider>
    </Router>
  );
}
