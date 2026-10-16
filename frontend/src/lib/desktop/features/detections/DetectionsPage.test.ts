import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, cleanup } from '@testing-library/svelte';
import DetectionsPage from './DetectionsPage.svelte';
import { fetchWithCSRF } from '$lib/utils/api';

vi.mock('$lib/utils/api', async importOriginal => ({
  ...(await importOriginal<typeof import('$lib/utils/api')>()),
  fetchWithCSRF: vi.fn(),
  // The filter panel loads its audio source list through api.get.
  api: { get: vi.fn().mockResolvedValue({ sources: [] }) },
}));

type FetchOptions = Parameters<typeof fetchWithCSRF>[1];

/** Page size used by every response, so offsets map to predictable ranges. */
const PAGE_SIZE = 25;
const TOTAL_RESULTS = 200;
const LOADING_TEXT = 'detections.aria.loadingResults';

interface Deferred {
  resolve: (_value: unknown) => void;
  signal?: AbortSignal;
}

/** A detections response for the page starting at `offset`, with no rows. */
function pageResponse(offset: number) {
  return {
    data: [],
    total: TOTAL_RESULTS,
    limit: PAGE_SIZE,
    current_page: offset / PAGE_SIZE + 1,
    total_pages: TOTAL_RESULTS / PAGE_SIZE,
  };
}

/** Let pending promise callbacks and the resulting DOM update run. */
function settle() {
  return new Promise(resolve => setTimeout(resolve));
}

/** The page number the pager marks as current. */
function currentPage() {
  return screen.getByRole('button', { current: 'page' }).textContent.trim();
}

describe('DetectionsPage request ordering', () => {
  let pending: Deferred[];

  beforeEach(() => {
    pending = [];
    window.history.replaceState({}, '', `/ui/detections?numResults=${PAGE_SIZE}`);
    // Each call stays open until the test resolves it, and deliberately ignores
    // the abort signal so the page's own guard is what keeps stale data out.
    vi.mocked(fetchWithCSRF).mockImplementation(
      (_url: string, options?: FetchOptions) =>
        new Promise(resolve => {
          pending.push({ resolve, signal: options?.signal ?? undefined });
        })
    );
  });

  afterEach(() => {
    cleanup();
    vi.mocked(fetchWithCSRF).mockReset();
  });

  /** Move to another page the way the browser's Back/Forward does. */
  function popTo(offset: number) {
    window.history.pushState({}, '', `/ui/detections?numResults=${PAGE_SIZE}&offset=${offset}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }

  it('keeps the latest page when an earlier request answers last', async () => {
    const firstOffset = PAGE_SIZE;
    const latestOffset = PAGE_SIZE * 3;

    render(DetectionsPage);
    await waitFor(() => expect(pending).toHaveLength(1));

    // Settle the initial load so there is a page on screen.
    pending[0].resolve(pageResponse(0));
    await waitFor(() => expect(screen.queryByText(LOADING_TEXT)).not.toBeInTheDocument());

    popTo(firstOffset);
    popTo(latestOffset);
    await waitFor(() => expect(pending).toHaveLength(3));

    const [, superseded, latest] = pending;

    // The superseded request answering first must neither show its page nor end
    // the loading state while the latest one is still out.
    superseded.resolve(pageResponse(firstOffset));
    await settle();
    expect(screen.getByText(LOADING_TEXT)).toBeInTheDocument();
    expect(currentPage()).toBe('1');

    latest.resolve(pageResponse(latestOffset));
    await waitFor(() => expect(screen.queryByText(LOADING_TEXT)).not.toBeInTheDocument());
    expect(currentPage()).toBe('4');

    // The superseded request was also cancelled, not just ignored.
    expect(superseded.signal?.aborted).toBe(true);
    expect(latest.signal?.aborted).toBe(false);
  });

  it('does not report an aborted request as an error', async () => {
    render(DetectionsPage);
    await waitFor(() => expect(pending).toHaveLength(1));

    popTo(PAGE_SIZE);
    await waitFor(() => expect(pending).toHaveLength(2));

    const [superseded, latest] = pending;
    // The real fetchWithCSRF rethrows the caller's AbortError when its signal
    // fires; resolving with a rejected promise rejects the pending call.
    superseded.resolve(Promise.reject(new DOMException('Aborted', 'AbortError')));
    await settle();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByText(LOADING_TEXT)).toBeInTheDocument();

    latest.resolve(pageResponse(PAGE_SIZE));
    await waitFor(() => expect(screen.queryByText(LOADING_TEXT)).not.toBeInTheDocument());
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(superseded.signal?.aborted).toBe(true);
  });
});
