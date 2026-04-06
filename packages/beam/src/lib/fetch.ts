import i18n from '../i18n';

/** Default to the current browser origin in embedded/BFF mode; override with `VITE_API_BASE_URL`. */
function resolveApiOrigin(): string {
  const explicit = import.meta.env.VITE_API_BASE_URL;
  if (explicit !== undefined && explicit !== '') {
    return explicit;
  }
  const useProxy =
    import.meta.env.VITE_DEV_API_PROXY === 'true' ||
    import.meta.env.VITE_DEV_API_PROXY === '1';
  if (typeof window !== 'undefined' && window.location?.origin) {
    if (useProxy || !import.meta.env.DEV) {
      return window.location.origin;
    }
  }
  return 'http://localhost:8081';
}

const API_BASE_URL = resolveApiOrigin();
const envTimeout = import.meta.env.VITE_API_TIMEOUT;
const parsedTimeout = envTimeout ? parseInt(envTimeout, 10) : NaN;
const API_TIMEOUT = !isNaN(parsedTimeout) ? parsedTimeout : 100000;

export class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
    public details?: unknown,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

// --- API Fetch Helper ---
export async function apiFetch<T>(url: string, options?: RequestInit): Promise<T> {
  // If caller supplies a signal, we *merge* it with our timeout controller.
  const externalSignal = options?.signal ?? null;
  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), API_TIMEOUT);

  // If the caller's signal aborts, abort ours too.
  if (externalSignal) {
    if (externalSignal.aborted) controller.abort();
    else externalSignal.addEventListener('abort', () => controller.abort(), { once: true });
  }

  try {
    const headers = new Headers(options?.headers);
    const isFormData = options?.body instanceof FormData;

    if (!isFormData && !headers.has('Content-Type')) {
      headers.set('Content-Type', 'application/json');
    }

    headers.set('Accept-Language', i18n.language);

    const response = await fetch(new URL(url, API_BASE_URL).toString(), {
      ...options,
      headers,
      // Use our controller so timeout *and* external abort both cancel the request.
      signal: controller.signal,
    });
    clearTimeout(timeoutId);

    if (!response.ok) {
      let errorMessage = i18n.t('errors.unknown');
      let errorDetails = null;
      const contentType = response.headers.get('Content-Type');

      try {
        if (contentType?.includes('application/json')) {
          const errorBody = await response.json();
          const apiError = (errorBody as any).error;

          if (apiError && typeof apiError === 'object') {
            errorMessage = apiError.message || i18n.t('errors.unknown');
            errorDetails = {
              type: apiError.type,
              code: apiError.code,
              param: apiError.param,
              raw: errorBody,
            };
          } else {
            errorMessage = (errorBody as any).message || JSON.stringify(errorBody);
            errorDetails = { raw: errorBody };
          }
        } else {
          errorMessage = await response.text();
        }
      } catch (error) {
        errorMessage = response.statusText || errorMessage;
      }

      throw new ApiError(errorMessage, response.status, errorDetails ?? undefined);
    }

    try {
      return await response.json();
    } catch (error) {
      throw new ApiError(i18n.t('errors.invalidResponse'), response.status, { cause: error });
    }
  } catch (error) {
    clearTimeout(timeoutId);

    if (error instanceof ApiError) throw error;

    if (error instanceof DOMException && error.name === 'AbortError') {
      // Distinguish timeout vs. manual abort is optional; message is fine as-is.
      throw new ApiError(i18n.t('errors.timeout'), 0);
    }

    if (error instanceof Error) {
      throw new ApiError(error.message, 0);
    }

    throw new ApiError(i18n.t('errors.unknown'), 0);
  }
}

/** GET binary body (e.g. VFS download); same origin, timeout, and error handling as apiFetch. */
export async function apiFetchBinary(url: string, init?: RequestInit): Promise<ArrayBuffer> {
  const externalSignal = init?.signal ?? null;
  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), API_TIMEOUT);
  if (externalSignal) {
    if (externalSignal.aborted) controller.abort();
    else externalSignal.addEventListener('abort', () => controller.abort(), { once: true });
  }
  try {
    const headers = new Headers(init?.headers);
    headers.set('Accept-Language', i18n.language);
    const response = await fetch(new URL(url, API_BASE_URL).toString(), {
      ...init,
      credentials: 'same-origin',
      headers,
      signal: controller.signal,
    });
    clearTimeout(timeoutId);
    if (!response.ok) {
      let errorMessage = i18n.t('errors.unknown');
      try {
        errorMessage = await response.text();
      } catch {
        errorMessage = response.statusText || errorMessage;
      }
      throw new ApiError(errorMessage, response.status);
    }
    return await response.arrayBuffer();
  } catch (error) {
    clearTimeout(timeoutId);
    if (error instanceof ApiError) throw error;
    if (error instanceof DOMException && error.name === 'AbortError') {
      throw new ApiError(i18n.t('errors.timeout'), 0);
    }
    if (error instanceof Error) {
      throw new ApiError(error.message, 0);
    }
    throw new ApiError(i18n.t('errors.unknown'), 0);
  }
}
