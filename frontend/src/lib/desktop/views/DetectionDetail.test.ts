import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { waitFor, cleanup } from '@testing-library/svelte';
import { createComponentTestFactory } from '../../../test/render-helpers';
import DetectionDetail from './DetectionDetail.svelte';
import AudioPlayer from '$lib/desktop/components/media/AudioPlayer.svelte';
import type { Detection } from '$lib/types/detection.types';

// Heavy / context-dependent children are not relevant to the fetch-race logic.
vi.mock('$lib/desktop/components/media/AudioPlayer.svelte');
vi.mock('$lib/desktop/components/data/ConfidenceCircle.svelte');
vi.mock('$lib/desktop/components/data/WeatherDetails.svelte');
vi.mock('$lib/desktop/features/dashboard/components/SourceBadge.svelte');
vi.mock('$lib/desktop/components/ui/VerificationBadges.svelte');

const detailTest = createComponentTestFactory(DetectionDetail);

/** Build a minimal valid Detection for the detail view. */
function makeDetection(overrides: Partial<Detection>): Detection {
  return {
    id: 1,
    date: '2024-01-01',
    time: '10:00:00',
    timestamp: '2024-01-01T10:00:00Z',
    beginTime: '2024-01-01T10:00:00Z',
    endTime: '2024-01-01T10:00:03Z',
    speciesCode: 'spc',
    scientificName: 'Default scientific',
    commonName: 'Default common',
    confidence: 0.9,
    verified: 'unverified',
    locked: false,
    ...overrides,
  };
}

// Sentinel scientific names referenced by both the fixtures and the assertions,
// so a typo cannot silently desync the two.
const FRESH_SCIENTIFIC = 'Fresh-sci-B';
const STALE_SCIENTIFIC = 'Stale-sci-A';

/** Minimal fetch Response stub carrying a JSON body. */
function jsonResponse(body: unknown): Response {
  return {
    ok: true,
    status: 200,
    statusText: 'OK',
    headers: new Headers({ 'content-type': 'application/json' }),
    json: () => Promise.resolve(body),
    // Serialize lazily and reject (never throw synchronously) so this
    // Promise-returning method honors its contract even on a non-serializable body.
    text: () => {
      try {
        return Promise.resolve(JSON.stringify(body));
      } catch (error) {
        return Promise.reject(error instanceof Error ? error : new Error(String(error)));
      }
    },
  } as unknown as Response;
}

describe('DetectionDetail stale-response race (#978)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  // Regression: navigating from detection A to B while A's request is still in
  // flight must not let A's late response overwrite B. The fix captures the
  // AbortController signal locally and checks the captured signal (not the shared
  // controller reference, which by then points at B's non-aborted controller).
  it('does not let a stale detection response overwrite a newer one', async () => {
    let resolveStale!: (r: Response) => void;
    const staleResponse = new Promise<Response>(resolve => {
      resolveStale = resolve;
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = String(input);
        // Detection A: held in flight until we resolve it manually (after switching to B).
        if (url.includes('/api/v2/detections/det-a')) {
          return staleResponse;
        }
        // Detection B: resolves immediately and becomes the current detection.
        if (url.includes('/api/v2/detections/det-b')) {
          return Promise.resolve(
            jsonResponse(
              makeDetection({ id: 2, scientificName: FRESH_SCIENTIFIC, commonName: 'Fresh B' })
            )
          );
        }
        // Secondary species/taxonomy/attribution endpoints: irrelevant here.
        return Promise.resolve(jsonResponse({}));
      })
    );

    const { container, rerender } = detailTest.render({ detectionId: 'det-a' });

    // Switch to detection B before A resolves.
    await rerender({ detectionId: 'det-b' });
    await waitFor(() => {
      expect(container.textContent).toContain(FRESH_SCIENTIFIC);
    });

    // A's response now arrives late; the captured-signal guard must drop it.
    resolveStale(
      jsonResponse(
        makeDetection({ id: 1, scientificName: STALE_SCIENTIFIC, commonName: 'Stale A' })
      )
    );
    // Flush the production stale-handling path: await the promise it awaits, then
    // a macrotask so every microtask hop (response.json, the captured-signal
    // guard) and the Svelte DOM flush complete before asserting. A microtask-only
    // flush (await tick) under-drains and lets the negative assertion fire early.
    await staleResponse;
    await new Promise(resolve => setTimeout(resolve, 0));

    expect(container.textContent).toContain(FRESH_SCIENTIFIC);
    expect(container.textContent).not.toContain(STALE_SCIENTIFIC);
  });
});

describe('DetectionDetail audio player', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  // The player's own download button and Export menu fetch audioUrl, so it must stay the
  // ID-based endpoint (whose Content-Disposition header supplies the canonical filename).
  it('passes the ID-based audio endpoint to the player without disabling downloads', async () => {
    const detection = makeDetection({
      id: 1239,
      scientificName: 'Phalaenoptilus nuttallii',
      commonName: 'Common Poorwill',
      clipName: 'phalaenoptilus_nuttallii_88p_20260720T051601Z.m4a',
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        if (String(input).includes('/api/v2/detections/1239')) {
          return Promise.resolve(jsonResponse(detection));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    detailTest.render({ detectionId: '1239' });

    await waitFor(() => {
      expect(vi.mocked(AudioPlayer)).toHaveBeenCalled();
    });

    // Svelte 5 components are called as (anchor, props).
    const props = vi.mocked(AudioPlayer).mock.calls.at(-1)?.[1];
    expect(props?.audioUrl).toBe('/api/v2/audio/1239');
    expect(props?.detectionId).toBe('1239');
    // DetectionDetail relies on the player's default (showDownload = true).
    expect(props?.showDownload).not.toBe(false);
  });
});

describe('DetectionDetail new-this-season badge', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  function renderWithSeason(currentSeason: string) {
    const detection = makeDetection({ id: 55, isNewThisSeason: true, currentSeason });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        if (String(input).includes('/api/v2/detections/55')) {
          return Promise.resolve(jsonResponse(detection));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    return detailTest.render({ detectionId: '55' });
  }

  // The test i18n setup returns keys, so a localized name shows up as its message key.
  it('shows the localized season name', async () => {
    const { container } = renderWithSeason('spring');

    await waitFor(() => {
      expect(container.textContent).toContain(
        ': settings.species.tracking.seasonal.seasons.spring'
      );
    });
  });

  it('shows the localized name for equatorial seasons', async () => {
    const { container } = renderWithSeason('wet1');

    await waitFor(() => {
      expect(container.textContent).toContain(': settings.species.tracking.seasonal.seasons.wet1');
    });
  });

  it('capitalizes a season the UI has no name for', async () => {
    const { container } = renderWithSeason('monsoon');

    await waitFor(() => {
      expect(container.textContent).toContain(': Monsoon');
    });
  });
});

describe('DetectionDetail tab URL', () => {
  // The test setup replaces window.location with a static stub, so each test installs
  // its own and watches history.replaceState for the URL the view writes back.
  const originalLocationDescriptor = Object.getOwnPropertyDescriptor(window, 'location');

  function setLocation(pathWithQuery: string) {
    const url = new URL(pathWithQuery, 'http://localhost:3000');
    Object.defineProperty(window, 'location', {
      configurable: true,
      writable: true,
      value: {
        href: url.href,
        origin: url.origin,
        pathname: url.pathname,
        search: url.search,
        hash: '',
      },
    });
  }

  function stubDetectionFetch() {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        if (String(input).includes('/api/v2/detections/66')) {
          return Promise.resolve(jsonResponse(makeDetection({ id: 66 })));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );
  }

  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    if (originalLocationDescriptor) {
      Object.defineProperty(window, 'location', originalLocationDescriptor);
    }
  });

  // Bookmarks from before the overview tab was retired must still open the page.
  it('falls back to notes for the retired overview tab and drops the parameter', async () => {
    setLocation('/ui/detections/66?tab=overview');
    const replaceState = vi.spyOn(window.history, 'replaceState').mockImplementation(() => {});
    stubDetectionFetch();

    const { container } = detailTest.render({ detectionId: '66' });

    await waitFor(() => {
      expect(container.querySelector('#tab-panel-notes')).not.toBeNull();
    });
    expect(container.querySelector('#tab-overview')).toBeNull();
    await waitFor(() => {
      expect(replaceState).toHaveBeenCalledWith(null, '', '/ui/detections/66');
    });
  });

  it('keeps a valid tab from the URL', async () => {
    setLocation('/ui/detections/66?tab=history');
    const replaceState = vi.spyOn(window.history, 'replaceState').mockImplementation(() => {});
    stubDetectionFetch();

    const { container } = detailTest.render({ detectionId: '66' });

    await waitFor(() => {
      expect(container.querySelector('#tab-panel-history')).not.toBeNull();
    });
    // The URL already matches the active tab, so it is left alone.
    expect(replaceState).not.toHaveBeenCalled();
  });

  it('labels the media region as an audio recording when the detection has a clip', async () => {
    const detection = makeDetection({
      id: 1241,
      scientificName: 'Phalaenoptilus nuttallii',
      commonName: 'Common Poorwill',
      clipName: 'phalaenoptilus_nuttallii_88p_20260720T051601Z.m4a',
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        if (String(input).includes('/api/v2/detections/1241')) {
          return Promise.resolve(jsonResponse(detection));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    const { container } = detailTest.render({ detectionId: '1241' });

    await waitFor(() => {
      expect(container.querySelector('section[aria-labelledby="media-heading"]')).not.toBeNull();
    });

    expect(
      container
        .querySelector('section[aria-labelledby="media-heading"] [role="region"]')
        ?.getAttribute('aria-label')
    ).toBe('detections.detail.aria.audioRecordingFor');
    // Positive control: the automocked player records its render, so the negative assertion in the
    // spectrogram-only test can fail.
    expect(vi.mocked(AudioPlayer)).toHaveBeenCalled();
  });

  it('links to the species All About Birds sounds page next to the recording', async () => {
    const detection = makeDetection({
      id: 1239,
      scientificName: 'Phalaenoptilus nuttallii',
      commonName: 'Common Poorwill',
      clipName: 'phalaenoptilus_nuttallii_88p_20260720T051601Z.m4a',
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        if (String(input).includes('/api/v2/detections/1239')) {
          return Promise.resolve(jsonResponse(detection));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    const { container } = detailTest.render({ detectionId: '1239' });

    await waitFor(() => {
      expect(container.querySelector('.media-heading-row a.media-compare-link')).not.toBeNull();
    });

    const compareLink = container.querySelector<HTMLAnchorElement>(
      '.media-heading-row a.media-compare-link'
    );
    expect(compareLink?.getAttribute('href')).toBe(
      'https://www.allaboutbirds.org/guide/Common_Poorwill/sounds'
    );
    expect(compareLink).toHaveAttribute('target', '_blank');
    expect(compareLink).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('links to the species All About Birds guide and Wikipedia article in the identity card', async () => {
    const detection = makeDetection({
      id: 1240,
      scientificName: 'Dumetella carolinensis',
      commonName: 'Gray Catbird',
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        if (String(input).includes('/api/v2/detections/1240')) {
          return Promise.resolve(jsonResponse(detection));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    const { container } = detailTest.render({ detectionId: '1240' });

    await waitFor(() => {
      expect(container.querySelector('.species-reference-links')).not.toBeNull();
    });

    const referenceLinks = container.querySelectorAll<HTMLAnchorElement>(
      '.species-reference-links a'
    );
    expect(referenceLinks).toHaveLength(2);
    expect(referenceLinks[0].getAttribute('href')).toBe(
      'https://www.allaboutbirds.org/guide/Gray_Catbird/id'
    );
    expect(referenceLinks[1].getAttribute('href')).toBe(
      'https://en.wikipedia.org/wiki/Gray_Catbird'
    );
    referenceLinks.forEach(link => {
      expect(link).toHaveAttribute('target', '_blank');
      expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    });
  });

  // A blank common name would build a guide URL for a page that does not exist,
  // so the links are omitted rather than rendered broken.
  it('omits the reference links when the detection has no common name', async () => {
    const detection = makeDetection({
      id: 1241,
      scientificName: 'Dumetella carolinensis',
      commonName: '   ',
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        if (String(input).includes('/api/v2/detections/1241')) {
          return Promise.resolve(jsonResponse(detection));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    const { container } = detailTest.render({ detectionId: '1241' });

    // Anchor on the loaded detection's own text: the skeleton also renders a
    // .hero-confidence, so waiting on that would assert before the data arrives.
    await waitFor(() => {
      expect(container.querySelector('.species-scientific-name')?.textContent).toContain(
        'Dumetella carolinensis'
      );
    });

    expect(container.querySelector('.species-reference-links')).toBeNull();
  });
});

describe('DetectionDetail spectrogram-only detection', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('shows the kept spectrogram image without a player or download link', async () => {
    const detection = makeDetection({
      id: 1240,
      scientificName: 'Phalaenoptilus nuttallii',
      commonName: 'Common Poorwill',
      clipName: '',
      spectrogramOnly: true,
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        if (String(input).includes('/api/v2/detections/1240')) {
          return Promise.resolve(jsonResponse(detection));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    const { container } = detailTest.render({ detectionId: '1240' });

    await waitFor(() => {
      expect(container.querySelector('img.spectrogram-img')).not.toBeNull();
    });

    expect(container.querySelector('img.spectrogram-img')?.getAttribute('src')).toContain(
      '/api/v2/spectrogram/1240?size=lg&raw=false'
    );
    expect(container.querySelector('a.meta-download')).toBeNull();
    expect(container.querySelector('#media-heading')).not.toBeNull();
    // The region names the image, not an audio recording that no longer exists.
    expect(
      container
        .querySelector('section[aria-labelledby="media-heading"] [role="region"]')
        ?.getAttribute('aria-label')
    ).toBe('components.audio.spectrogramForSpecies');
    expect(vi.mocked(AudioPlayer)).not.toHaveBeenCalled();
  });
});

describe('DetectionDetail rarity location coordinates', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  // Render the detail view with a species-rarity payload, stubbing the detection and the
  // species-info endpoint (which carries rarity) and ignoring everything else.
  function renderWithRarity(rarity: Record<string, unknown>) {
    const detection = makeDetection({
      id: 77,
      scientificName: 'Turdus merula',
      commonName: 'Blackbird',
    });
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes('/api/v2/detections/77')) {
          return Promise.resolve(jsonResponse(detection));
        }
        // The species-info endpoint (not /species/taxonomy) supplies rarity.
        if (url.includes('/api/v2/species?scientific_name=')) {
          return Promise.resolve(jsonResponse({ scientific_name: 'Turdus merula', rarity }));
        }
        return Promise.resolve(jsonResponse({}));
      })
    );
    return detailTest.render({ detectionId: '77' });
  }

  function raritySection(container: HTMLElement): HTMLElement | null {
    return container.querySelector<HTMLElement>('.detection-facts .fact-rarity');
  }

  // A station configured at exactly 0.0 latitude/longitude (equator / prime meridian). The
  // API now sends latitude:0 / longitude:0 (previously dropped by float64+omitempty), and
  // the component must render the based-on-location line without throwing on toFixed(0).
  it('renders the location line when coordinates are exactly 0', async () => {
    const { container } = renderWithRarity({
      status: 'rare',
      score: 0.08,
      location_based: true,
      latitude: 0,
      longitude: 0,
      date: '2024-06-15',
      threshold_applied: 0.05,
    });

    await waitFor(() => {
      expect(raritySection(container)).not.toBeNull();
    });

    // The based-on-location line is the rarity fact's only note.
    expect(raritySection(container)?.querySelectorAll('.fact-note')).toHaveLength(1);
  });

  // Defensive: if the API ever reports location_based while omitting the coordinates (a
  // contract violation), the != null guard must hide the line instead of crashing on
  // undefined.toFixed. The rarity status still renders; the score is hidden for 'unknown'.
  it('omits the location line and does not crash when coordinates are missing', async () => {
    const { container } = renderWithRarity({
      status: 'unknown',
      score: 0,
      location_based: true,
      date: '2024-06-15',
      threshold_applied: 0.05,
    });

    await waitFor(() => {
      expect(raritySection(container)).not.toBeNull();
    });

    const section = raritySection(container);
    expect(section?.querySelector('.rarity-label')).not.toBeNull();
    expect(section?.querySelectorAll('.fact-note')).toHaveLength(0);
    // A 0% score next to "Unknown" carries no information, so it is hidden.
    expect(section?.querySelector('.rarity-score')).toBeNull();
  });
});

describe('DetectionDetail card layout', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('shows species, status and detection facts in one header card above the recording', async () => {
    const detection = makeDetection({
      id: 88,
      scientificName: 'Turdus merula',
      commonName: 'Blackbird',
      clipName: 'turdus_merula_90p_20240101T100000Z.m4a',
      isNewSpecies: true,
    });
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes('/api/v2/detections/88')) {
          return Promise.resolve(jsonResponse(detection));
        }
        if (url.includes('/api/v2/species/taxonomy?scientific_name=')) {
          return Promise.resolve(
            jsonResponse({
              taxonomy: {
                kingdom: 'Animalia',
                phylum: 'Chordata',
                class: 'Aves',
                order: 'Passeriformes',
                family: 'Turdidae',
                genus: 'Turdus',
                species: 'Turdus merula',
              },
            })
          );
        }
        return Promise.resolve(jsonResponse({}));
      })
    );

    const { container } = detailTest.render({ detectionId: '88' });

    await waitFor(() => {
      expect(container.querySelector('.detection-header .taxonomy-path')).not.toBeNull();
    });

    // Exactly one header card holds the species, its lineage, confidence and facts.
    expect(container.querySelectorAll('.detection-header')).toHaveLength(1);
    const header = container.querySelector<HTMLElement>('.detection-header');
    const lineage = header?.querySelector<HTMLElement>('.taxonomy-path');
    expect(lineage?.textContent).toContain('Aves');
    expect(lineage?.textContent).toContain('Passeriformes');
    expect(lineage?.textContent).toContain('Turdidae');
    // Genus and species are already in the binomial, so the path stops at family.
    expect(lineage?.textContent).not.toContain('Turdus');
    expect(header?.querySelector('.header-confidence')).not.toBeNull();
    expect(header?.querySelector('.detection-facts')).not.toBeNull();
    // Tracking flags are badges in the header, not a separate overview tab.
    expect(header?.querySelector('.header-badges')?.textContent).toContain(
      'species.tracking.newSpecies'
    );
    expect(header?.querySelector('[role="tablist"]')).toBeNull();
    // The recording card's Export menu (or the player's own download button)
    // already offers the original file, so the header has no download link.
    expect(header?.querySelector('a[download]')).toBeNull();

    // Tabs follow the recording, and there is no overview tab any more.
    const media = container.querySelector<HTMLElement>('.media-card');
    const tabs = container.querySelector<HTMLElement>('.detail-tabs-card');
    expect(media).not.toBeNull();
    expect(tabs).not.toBeNull();
    const position = tabs ? media?.compareDocumentPosition(tabs) : undefined;
    expect((position ?? 0) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(container.querySelectorAll('[role="tablist"]')).toHaveLength(1);
    expect(container.querySelector('#tab-overview')).toBeNull();
    expect(container.querySelector('#tab-panel-notes')).not.toBeNull();
  });
});
