import { Card, Page } from '@contenox/ui';
import { LoginForm } from './LoginForm';

export default function AuthPage() {
  return (
    <Page bodyScroll="hidden">
      <div className="flex min-h-screen flex-col justify-start py-16">
        <Card className="w-full max-w-4xl min-w-xs place-self-center" variant="filled">
          <LoginForm />
        </Card>
      </div>
    </Page>
  );
}
