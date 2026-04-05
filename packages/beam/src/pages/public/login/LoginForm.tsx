import { Button, Form, FormField, Input, PasswordInput } from '@contenox/ui';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useLogin } from '../../../hooks/useLogin';

export function LoginForm() {
  const { t } = useTranslation();
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [localError, setLocalError] = useState<string | null>(null);

  const { mutate: loginMutate, isPending: isLoginPending, error: loginError } = useLogin();
  const isFormValid = email.trim() !== '' && password.trim() !== '';

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!isFormValid) {
      setLocalError(t('login.all_fields_required', 'All fields are required.'));
      return;
    }

    setLocalError(null);
    loginMutate({ email, password });
  };

  return (
    <Form
      title={t('login.title')}
      onSubmit={handleSubmit}
      error={
        localError ||
        (loginError
          ? t('login.error', 'Login error: {{error}}', { error: loginError.message })
          : undefined)
      }
      onError={errorMsg => console.error('Form error:', errorMsg)}
      actions={
        <>
          <Button type="submit" variant="primary" disabled={isLoginPending || !isFormValid}>
            {isLoginPending ? t('login.loading') : t('login.submit')}
          </Button>
        </>
      }>
      <FormField label={t('login.user_name')} required>
        <Input value={email} onChange={e => setEmail(e.target.value)} disabled={isLoginPending} />
      </FormField>
      <FormField label={t('login.user_password')} required>
        <PasswordInput
          value={password}
          onChange={e => setPassword(e.target.value)}
          disabled={isLoginPending}
        />
      </FormField>
    </Form>
  );
}
