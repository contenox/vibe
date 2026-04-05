import { Navigate } from 'react-router-dom';

/** Internal tool: land users on chats instead of a marketing home. */
export default function HomeRedirect() {
  return <Navigate to="/chats" replace />;
}
