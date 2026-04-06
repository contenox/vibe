import { Navigate } from 'react-router-dom';

/** Internal tool: land users on chat workspace instead of a marketing home. */
export default function HomeRedirect() {
  return <Navigate to="/chat" replace />;
}
