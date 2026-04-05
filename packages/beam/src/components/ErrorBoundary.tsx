import { Component, ErrorInfo, ReactNode } from 'react';

interface Props {
  children: ReactNode;
  fallback?: ReactNode | ((error: Error, reset: () => void) => ReactNode);
}

interface State {
  error: Error | null;
}

export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('ErrorBoundary caught:', error, info);
  }

  reset = () => this.setState({ error: null });

  render() {
    if (this.state.error) {
      const { fallback } = this.props;
      if (typeof fallback === 'function') {
        return fallback(this.state.error, this.reset);
      }
      return (
        fallback ?? (
          <div className="flex min-h-screen items-center justify-center">
            <div className="text-center">
              <h1 className="mb-4 text-2xl font-semibold text-text dark:text-dark-text">
                Something went wrong
              </h1>
              <p className="mb-4 text-text-muted dark:text-dark-text-muted">
                {this.state.error.message}
              </p>
              <button
                onClick={this.reset}
                className="rounded-lg bg-primary-500 px-4 py-2 text-white hover:bg-primary-600"
              >
                Try again
              </button>
            </div>
          </div>
        )
      );
    }
    return this.props.children;
  }
}
