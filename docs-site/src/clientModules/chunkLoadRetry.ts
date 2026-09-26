/**
 * Retry failed lazy-chunk loads, then offer a reload.
 *
 * Every lazy import on the site — a route's page bundle, the Mermaid library
 * (~650 KB, pulled by any page with a diagram, which includes every ADR and
 * spec page through its "Related Artifacts" graph), and Mermaid's per-diagram
 * chunks — goes through the bundler's `__webpack_require__.e(chunkId)`. On a
 * flaky connection one of those requests can stall until the bundler's
 * 120-second timeout and reject with "Loading chunk N failed (timeout)", and
 * the bundler never tries again: the diagram or page is simply broken.
 *
 * This wraps `__webpack_require__.e` so a failed chunk is requested again a
 * few times with backoff. That is safe because both webpack's and rspack's
 * JSONP loaders clear a failed chunk's bookkeeping before rejecting, so the
 * next call issues a fresh request. A stalled TCP connection is exactly what
 * a fresh request fixes. If every attempt fails, a small banner offers a
 * reload, and the original error still propagates so error boundaries behave
 * as before.
 *
 * Works under rspack (Docusaurus Faster, the default with `future.v4`) and
 * webpack alike: both expose the same runtime object to module code.
 */
import ExecutionEnvironment from '@docusaurus/ExecutionEnvironment';

type EnsureChunk = ((chunkId: string | number) => Promise<unknown>) & {
  __harnessRetry?: true;
};

declare const __webpack_require__: {e?: EnsureChunk} | undefined;

// Delay before each retry. Three retries after the first attempt.
const RETRY_DELAYS_MS = [1000, 3000, 8000];
// While the browser reports itself offline, wait for it to come back (up to
// this long) instead of burning a retry on a request that cannot succeed.
const OFFLINE_WAIT_MS = 30000;

const BANNER_ID = 'chunk-load-banner';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function waitUntilOnline(maxMs: number): Promise<void> {
  if (typeof navigator === 'undefined' || navigator.onLine !== false) {
    return Promise.resolve();
  }
  return new Promise((resolve) => {
    const done = () => {
      window.removeEventListener('online', done);
      clearTimeout(timer);
      resolve();
    };
    const timer = setTimeout(done, maxMs);
    window.addEventListener('online', done);
  });
}

export function isChunkLoadError(error: unknown): boolean {
  if (!error || typeof error !== 'object') return false;
  const {name, message} = error as {name?: string; message?: string};
  return (
    name === 'ChunkLoadError' ||
    /Loading (CSS )?chunk \S+ failed/i.test(String(message ?? ''))
  );
}

export function showReloadBanner(): void {
  if (document.getElementById(BANNER_ID)) return;
  const banner = document.createElement('div');
  banner.id = BANNER_ID;
  banner.className = 'chunk-load-banner';
  banner.setAttribute('role', 'alert');

  const text = document.createElement('span');
  text.textContent =
    'Part of this page did not load — the connection dropped or timed out.';

  const reload = document.createElement('button');
  reload.type = 'button';
  reload.className = 'chunk-load-banner__reload';
  reload.textContent = 'Reload page';
  reload.addEventListener('click', () => window.location.reload());

  const dismiss = document.createElement('button');
  dismiss.type = 'button';
  dismiss.className = 'chunk-load-banner__dismiss';
  dismiss.setAttribute('aria-label', 'Dismiss');
  dismiss.textContent = '×';
  dismiss.addEventListener('click', () => banner.remove());

  banner.append(text, reload, dismiss);
  document.body.appendChild(banner);
  reload.focus({preventScroll: true});
}

function install(): void {
  if (typeof __webpack_require__ === 'undefined') return;
  const runtime = __webpack_require__;
  const original = runtime.e;
  if (typeof original !== 'function' || original.__harnessRetry) return;

  const retrying: EnsureChunk = async (chunkId) => {
    for (let attempt = 0; ; attempt += 1) {
      try {
        return await original(chunkId);
      } catch (error) {
        if (!isChunkLoadError(error) || attempt >= RETRY_DELAYS_MS.length) {
          if (isChunkLoadError(error)) showReloadBanner();
          throw error;
        }
        await sleep(RETRY_DELAYS_MS[attempt]);
        await waitUntilOnline(OFFLINE_WAIT_MS);
      }
    }
  };
  retrying.__harnessRetry = true;
  runtime.e = retrying;
}

if (ExecutionEnvironment.canUseDOM) {
  install();
}
