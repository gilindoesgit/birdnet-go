import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import DetectionHistory from './DetectionHistory.svelte';

const EMPTY_HEATMAP = { dates: [], slotResolutionMinutes: 30, cells: {} };

describe('DetectionHistory', () => {
  const fetchMock = vi.fn();

  beforeEach(() => {
    fetchMock.mockReset();
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => EMPTY_HEATMAP,
    });
    vi.stubGlobal('fetch', fetchMock);
  });

  it('requests the seasonal heatmap for the detection species', async () => {
    render(DetectionHistory, { props: { scientificName: 'Turdus merula' } });

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain('/api/v2/analytics/time/heatmap');
    expect(new URL(url, 'http://localhost').searchParams.get('species')).toBe('Turdus merula');
  });

  it('refetches when the range changes', async () => {
    render(DetectionHistory, { props: { scientificName: 'Turdus merula' } });
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));

    const quarter = screen.getAllByRole('button', { pressed: false })[0];
    await fireEvent.click(quarter);

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
  });
});
