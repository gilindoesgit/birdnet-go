<!--
  DetectionDetail.svelte - Single Detection View Component

  Purpose: Display comprehensive details for a single bird detection

  Features:
  - Header card: thumbnail, names and class › order › family; review, source and
    tracking badges; confidence; and one strip of date/time, weather, rarity and
    days since first seen
  - Audio player with large spectrogram visualization
  - Tabbed notes, history and review below the recording

  Props:
  - detectionId: string - The ID of the detection to display
-->
<script lang="ts">
  import ConfidenceCircle from '$lib/desktop/components/data/ConfidenceCircle.svelte';
  import WeatherDetails from '$lib/desktop/components/data/WeatherDetails.svelte';
  import AudioPlayer from '$lib/desktop/components/media/AudioPlayer.svelte';
  import SpectrogramImage from '$lib/desktop/components/media/SpectrogramImage.svelte';
  import VerificationBadges from '$lib/desktop/components/ui/VerificationBadges.svelte';
  import StatusPill from '$lib/desktop/components/ui/StatusPill.svelte';
  import ErrorAlert from '$lib/desktop/components/ui/ErrorAlert.svelte';
  import { handleBirdImageError } from '$lib/desktop/components/ui/image-utils.js';
  import { t, getLocale } from '$lib/i18n';
  import type { Detection, ImageAttribution } from '$lib/types/detection.types';
  import { hasReviewPermission, isAuthenticated } from '$lib/utils/auth';
  import { formatLocalDateTime } from '$lib/utils/date';
  import { buildAppUrl, getCurrentPathWithQuery } from '$lib/utils/urlHelpers';
  import { loggers } from '$lib/utils/logger';
  import { localizeSpeciesName } from '$lib/utils/speciesDisplay';
  import {
    getAllAboutBirdsSoundsUrl,
    getAllAboutBirdsUrl,
    getWikipediaUrl,
    hasSpeciesReferenceName,
  } from '$lib/utils/speciesLinks';
  import SourceBadge from '$lib/desktop/features/dashboard/components/SourceBadge.svelte';
  import DetectionHistory from '$lib/desktop/features/detections/components/DetectionHistory.svelte';
  import {
    Camera,
    Clock,
    StickyNote,
    Sun,
    Moon,
    Sunrise,
    Sunset,
    ExternalLink,
  } from '@lucide/svelte';

  // Interface definitions for API responses
  interface SpeciesRarity {
    status: string;
    score: number;
    location_based: boolean;
    // Optional: the API omits these when no location is configured (they are *float64
    // with omitempty server-side). The backend sends them whenever location_based is
    // true, but callers must still guard before toFixed() as defense in depth against
    // a contract violation.
    latitude?: number;
    longitude?: number;
  }

  interface SpeciesInfo {
    rarity?: SpeciesRarity;
    [key: string]: unknown;
  }

  interface TaxonomyHierarchy {
    kingdom: string;
    phylum: string;
    class: string;
    order: string;
    family: string;
    family_common?: string;
    genus: string;
    species: string;
  }

  interface Subspecies {
    scientific_name: string;
    common_name?: string;
  }

  interface TaxonomyInfo {
    taxonomy?: TaxonomyHierarchy;
    subspecies?: Subspecies[];
  }

  // ReviewCard component type (Svelte 5 component)
  type ReviewCardComponent =
    typeof import('$lib/desktop/components/review/ReviewCard.svelte').default;

  const logger = loggers.ui;

  // Constants
  const TAB_FOCUS_DELAY_MS = 50;
  type TabType = 'notes' | 'history' | 'review';
  // Shown when the URL has no tab (or an unknown one, such as the retired 'overview')
  const DEFAULT_TAB: TabType = 'notes';
  const VALID_TABS: readonly TabType[] = ['notes', 'history', 'review'];
  // Season keys the backend reports, each with a localized name in the settings messages
  const KNOWN_SEASONS: readonly string[] = [
    'spring',
    'summer',
    'fall',
    'winter',
    'wet1',
    'dry1',
    'wet2',
    'dry2',
  ];

  interface Props {
    detectionId?: string;
  }

  const { detectionId: detectionIdProp }: Props = $props();

  // Resolved detection ID - initialized by $effect below, not directly from prop
  // to ensure reactive updates work correctly
  let resolvedDetectionId = $state<string | undefined>(undefined);

  // State
  let activeTab = $state<TabType>(DEFAULT_TAB);

  // Dynamic review component loading
  let ReviewCard: ReviewCardComponent | null = $state(null);

  // Use the existing auth store pattern (same as DesktopSidebar)
  let canReview = $derived($hasReviewPermission);
  let clipExtractionEnabled = $derived($isAuthenticated);
  let detection = $state<Detection | null>(null);
  let speciesInfo = $state<SpeciesInfo | null>(null);
  let taxonomyInfo = $state<TaxonomyInfo | null>(null);
  let isLoadingDetection = $state(true);
  let isLoadingTaxonomy = $state(false);
  let detectionError = $state<string | null>(null);
  let imageAttribution = $state<ImageAttribution | null>(null);

  // Tabs in display order; review only for users allowed to review
  let visibleTabs = $derived<readonly TabType[]>(
    canReview ? VALID_TABS : VALID_TABS.filter(tab => tab !== 'review')
  );

  // Class › order › family for the header lineage (genus and species are in the binomial)
  let lineageRanks = $derived(
    taxonomyInfo?.taxonomy
      ? [
          { key: 'class', value: taxonomyInfo.taxonomy.class },
          { key: 'order', value: taxonomyInfo.taxonomy.order },
          { key: 'family', value: taxonomyInfo.taxonomy.family },
        ].filter(rank => rank.value)
      : []
  );

  // Localized season name, falling back to the capitalized key for an unknown season
  function seasonLabel(season: string): string {
    if (KNOWN_SEASONS.includes(season)) {
      return t(`settings.species.tracking.seasonal.seasons.${season}`);
    }
    return season.charAt(0).toUpperCase() + season.slice(1);
  }

  // Derived state for subspecies with proper typing
  let subspeciesList = $derived<Subspecies[]>(
    taxonomyInfo?.subspecies && Array.isArray(taxonomyInfo.subspecies)
      ? taxonomyInfo.subspecies
      : []
  );

  // AbortController for preventing race conditions
  let detectionController: AbortController | null = null;
  let speciesController: AbortController | null = null;
  let taxonomyController: AbortController | null = null;
  let attributionController: AbortController | null = null;

  // Validate detection ID to prevent path traversal attacks
  // Only allow alphanumeric characters, hyphens, and underscores
  function isValidDetectionId(id: string): boolean {
    return /^[a-zA-Z0-9_-]+$/.test(id);
  }

  // Resolve detection ID from URL if not provided via prop
  $effect(() => {
    if (!detectionIdProp) {
      const pathParts = window.location.pathname.split('/');
      const detectionIndex = pathParts.indexOf('detections');
      if (detectionIndex !== -1 && pathParts[detectionIndex + 1]) {
        const candidateId = pathParts[detectionIndex + 1];
        if (isValidDetectionId(candidateId)) {
          resolvedDetectionId = candidateId;
        } else {
          detectionError = t('detections.errors.noIdProvided');
        }
      }
    } else if (isValidDetectionId(detectionIdProp)) {
      resolvedDetectionId = detectionIdProp;
    } else {
      detectionError = t('detections.errors.noIdProvided');
    }
  });

  // Initialize tab from URL query parameter (with permission check for review tab)
  $effect(() => {
    const urlParams = new URLSearchParams(window.location.search);
    const tabParam = urlParams.get('tab');
    if (tabParam && VALID_TABS.includes(tabParam as TabType)) {
      activeTab = tabParam === 'review' && !canReview ? DEFAULT_TAB : (tabParam as TabType);
    }
  });

  // Fetch detection data when resolvedDetectionId changes
  $effect(() => {
    if (resolvedDetectionId) {
      fetchDetection();
    }

    return () => {
      detectionController?.abort();
      speciesController?.abort();
      taxonomyController?.abort();
      attributionController?.abort();
      cancelAttributionRetry();
    };
  });

  // Fetch detection data
  async function fetchDetection() {
    if (!resolvedDetectionId) {
      detectionError = t('detections.errors.noIdProvided');
      isLoadingDetection = false;
      return;
    }

    detectionController?.abort();
    const controller = new AbortController();
    detectionController = controller;
    const { signal } = controller;

    isLoadingDetection = true;
    detectionError = null;
    // Reset the detail and its dependent data so a newer detection never briefly
    // shows the previously viewed one's species/taxonomy/attribution while the
    // secondary fetches are still in flight.
    detection = null;
    speciesInfo = null;
    taxonomyInfo = null;
    imageAttribution = null;

    try {
      const response = await fetch(buildAppUrl(`/api/v2/detections/${resolvedDetectionId}`), {
        signal,
      });

      // Check the captured signal, not the shared controller: a newer request may
      // have replaced detectionController with a fresh, non-aborted instance, and
      // checking that would let this stale response overwrite newer data.
      if (signal.aborted) return;

      if (response.ok) {
        const data = (await response.json()) as Detection;
        if (signal.aborted) return;
        detection = data;
      } else {
        let errorMessage: string;
        switch (response.status) {
          case 404:
            errorMessage = t('detections.errors.notFound');
            break;
          case 403:
            errorMessage = t('detections.errors.noPermission');
            break;
          case 401:
            errorMessage = t('detections.errors.loginRequired');
            break;
          case 500:
          case 502:
          case 503:
            errorMessage = t('detections.errors.serverError');
            break;
          default:
            errorMessage = t('detections.errors.loadFailed', { status: response.status });
        }
        throw new Error(errorMessage);
      }

      if (detection) {
        fetchSpeciesInfo();
        fetchTaxonomy();
        fetchImageAttribution();
      }
    } catch (error) {
      if (signal.aborted || (error instanceof Error && error.name === 'AbortError')) {
        return;
      }
      detectionError =
        error instanceof Error ? error.message : t('detections.errors.loadFailed', { status: '' });
      logger.error('Error fetching detection:', error);
    } finally {
      // Only the request that still owns the controller may reset shared state; a
      // superseded request must not clear a newer request's loading flag/controller.
      if (detectionController === controller) {
        isLoadingDetection = false;
        detectionController = null;
      }
    }
  }

  /**
   * Delays before re-requesting attribution that answered "not resolved yet".
   * Deliberately shorter and fewer than the thumbnail's own schedule: attribution is
   * supporting detail, and the image itself is what the user is waiting on.
   */
  const ATTRIBUTION_RETRY_DELAYS_MS = [5000, 15000, 40000];

  /**
   * Pending 503 retry. Tracked separately from attributionController because the
   * controller is released as soon as its request settles: without this the scheduled
   * retry would still fire after the view was unmounted or navigated to a different
   * detection, since aborting the (already-nulled) controller could no longer reach it.
   */
  let attributionRetryTimer: ReturnType<typeof globalThis.setTimeout> | undefined;

  function cancelAttributionRetry() {
    if (attributionRetryTimer !== undefined) {
      globalThis.clearTimeout(attributionRetryTimer);
      attributionRetryTimer = undefined;
    }
  }

  // Fetch image attribution metadata
  async function fetchImageAttribution(attempt = 0) {
    if (!detection?.scientificName?.trim()) return;

    attributionController?.abort();
    cancelAttributionRetry();
    const controller = new AbortController();
    attributionController = controller;
    const { signal } = controller;

    try {
      const url = buildAppUrl(
        `/api/v2/media/species-image/info?name=${encodeURIComponent(detection.scientificName)}`
      );
      const response = await fetch(url, { signal });
      if (signal.aborted) return;
      if (response.ok) {
        const data = await response.json();
        if (signal.aborted) return;
        imageAttribution = data as ImageAttribution;
        return;
      }
      // 503 means the image is still being resolved in the background, not that it
      // has no attribution. Without a retry the CC-BY/CC-BY-SA photo that the
      // thumbnail's own retry recovers seconds later would be displayed with no
      // author or licence credit, which is a licensing problem and not merely a
      // cosmetic one.
      if (response.status === 503 && attempt < ATTRIBUTION_RETRY_DELAYS_MS.length) {
        const delay = ATTRIBUTION_RETRY_DELAYS_MS.at(attempt) ?? 0;
        attributionRetryTimer = globalThis.setTimeout(() => {
          attributionRetryTimer = undefined;
          if (!signal.aborted) void fetchImageAttribution(attempt + 1);
        }, delay);
        // Return before the finally block releases the controller, so this request's
        // signal stays reachable and an abort can still cancel the pending retry.
        return;
      }
    } catch (error) {
      if (signal.aborted || (error instanceof Error && error.name === 'AbortError')) return;
      // Attribution is non-critical - fail silently
    } finally {
      if (attributionController === controller) {
        attributionController = null;
      }
    }
  }

  // Fetch species information (public data - no auth required)
  async function fetchSpeciesInfo() {
    if (!detection?.scientificName?.trim()) return;

    speciesController?.abort();
    const controller = new AbortController();
    speciesController = controller;
    const { signal } = controller;

    try {
      const response = await fetch(
        buildAppUrl(
          `/api/v2/species?scientific_name=${encodeURIComponent(detection.scientificName)}`
        ),
        { signal }
      );
      if (signal.aborted) return;
      if (response.ok) {
        const data = await response.json();
        if (signal.aborted) return;
        speciesInfo = data;
      }
    } catch (error) {
      if (signal.aborted || (error instanceof Error && error.name === 'AbortError')) {
        return;
      }
      logger.error('Error fetching species info:', error);
    } finally {
      if (speciesController === controller) {
        speciesController = null;
      }
    }
  }

  // Fetch taxonomy information (public data - no auth required)
  async function fetchTaxonomy() {
    if (!detection?.scientificName?.trim()) return;

    taxonomyController?.abort();
    const controller = new AbortController();
    taxonomyController = controller;
    const { signal } = controller;

    isLoadingTaxonomy = true;
    try {
      const response = await fetch(
        buildAppUrl(
          `/api/v2/species/taxonomy?scientific_name=${encodeURIComponent(detection.scientificName)}`
        ),
        { signal }
      );
      if (signal.aborted) return;
      if (response.ok) {
        const data = await response.json();
        if (signal.aborted) return;
        taxonomyInfo = data;
      }
    } catch (error) {
      if (signal.aborted || (error instanceof Error && error.name === 'AbortError')) {
        return;
      }
      logger.error('Error fetching taxonomy info:', error);
    } finally {
      if (taxonomyController === controller) {
        isLoadingTaxonomy = false;
        taxonomyController = null;
      }
    }
  }

  // Dynamically load review component when user has review permission
  $effect(() => {
    if (canReview && !ReviewCard) {
      import('$lib/desktop/components/review/ReviewCard.svelte')
        .then(module => {
          ReviewCard = module.default;
          logger.debug('ReviewCard component loaded for authenticated user');
        })
        .catch(error => {
          logger.error('Failed to load ReviewCard component:', error);
        });
    }
  });

  // Handle review card completion
  function handleReviewComplete() {
    if (resolvedDetectionId) {
      fetchDetection();
    }
  }

  // Keyboard navigation handler for tab buttons
  function handleTabKeydown(e: KeyboardEvent) {
    const tabs = visibleTabs;
    const currentIndex = tabs.indexOf(activeTab);
    if (currentIndex === -1) return;

    if (e.key === 'ArrowRight') {
      e.preventDefault();
      activeTab = tabs[(currentIndex + 1) % tabs.length];
    } else if (e.key === 'ArrowLeft') {
      e.preventDefault();
      activeTab = tabs[(currentIndex - 1 + tabs.length) % tabs.length];
    }
  }

  // Focus management for tab switching
  $effect(() => {
    const activePanel = document.getElementById(`tab-panel-${activeTab}`);
    let timeoutId: ReturnType<typeof setTimeout> | null = null;

    if (activePanel && document.activeElement?.getAttribute('role') === 'tab') {
      timeoutId = setTimeout(() => {
        activePanel.focus();
      }, TAB_FOCUS_DELAY_MS);
    }

    return () => {
      if (timeoutId) clearTimeout(timeoutId);
    };
  });

  // URL state management - update URL when tab changes
  $effect(() => {
    if (typeof window !== 'undefined' && resolvedDetectionId) {
      const url = new URL(window.location.href);

      if (activeTab === DEFAULT_TAB) {
        url.searchParams.delete('tab');
      } else {
        url.searchParams.set('tab', activeTab);
      }

      const newUrl = url.pathname + url.search;
      if (newUrl !== getCurrentPathWithQuery()) {
        window.history.replaceState(null, '', newUrl);
      }
    }
  });

  // Handle browser back/forward navigation
  $effect(() => {
    if (typeof window !== 'undefined') {
      function handlePopState() {
        const urlParams = new URLSearchParams(window.location.search);
        const tabParam = urlParams.get('tab');
        if (tabParam && VALID_TABS.includes(tabParam as TabType)) {
          if (tabParam === 'review' && !canReview) {
            activeTab = DEFAULT_TAB;
          } else {
            activeTab = tabParam as TabType;
          }
        } else {
          activeTab = DEFAULT_TAB;
        }
      }

      window.addEventListener('popstate', handlePopState);

      return () => {
        window.removeEventListener('popstate', handlePopState);
      };
    }
  });
</script>

<!-- Snippets for better organization -->

{#snippet heroSection(det: Detection)}
  {@const displayName = localizeSpeciesName(det.scientificName, det.commonName)}
  {@const showDaysSinceFirst = det.daysSinceFirstSeen != null && det.daysSinceFirstSeen > 0}
  <!-- Header card: who was heard (photo, names, lineage), this detection's status
       badges and confidence, then one strip of labelled detection facts -->
  <section class="surface-card detection-header @container" aria-labelledby="species-heading">
    <div class="header-grid">
      <!-- Species thumbnail with credit overlay -->
      <div class="hero-thumbnail">
        <img
          src={buildAppUrl(
            `/api/v2/media/species-image?name=${encodeURIComponent(det.scientificName)}`
          )}
          alt={displayName}
          class="w-full h-full object-contain"
          onerror={handleBirdImageError}
          loading="eager"
        />
        {#if imageAttribution?.authorName}
          <div
            class="thumbnail-credit"
            aria-label={t('common.aria.imageCredit', { name: imageAttribution.authorName })}
          >
            <Camera size={10} class="credit-icon" />
            <span class="credit-text">{imageAttribution.authorName}</span>
            {#if imageAttribution.licenseName}
              <span class="credit-separator">·</span>
              {#if imageAttribution.licenseURL}
                <a
                  href={imageAttribution.licenseURL}
                  target="_blank"
                  rel="noopener noreferrer"
                  class="credit-license">{imageAttribution.licenseName}</a
                >
              {:else}
                <span class="credit-license">{imageAttribution.licenseName}</span>
              {/if}
            {/if}
          </div>
        {/if}
      </div>

      <!-- Names, lineage and status badges -->
      <div class="header-identity">
        <h1 id="species-heading" class="species-display-name">
          {displayName}
          <span class="sr-only">{t('detections.detail.aria.speciesHeadingSuffix')}</span>
        </h1>
        <!-- Scientific name, then class › order › family. Genus and species are
             omitted because the binomial already shows them. -->
        <p class="species-lineage">
          <span class="species-scientific-name">
            <span class="sr-only"
              >{t('detections.detail.aria.scientificName')}:
            </span>{det.scientificName}
          </span>
          {#if isLoadingTaxonomy}
            <span class="skeleton-line inline-block w-40 h-3" aria-hidden="true"></span>
          {:else if lineageRanks.length > 0}
            <span class="taxonomy-path" role="group" aria-label={t('species.taxonomy.hierarchy')}>
              {#each lineageRanks as rank, index (rank.key)}
                {#if index > 0}<span class="taxonomy-sep" aria-hidden="true">›</span>{/if}
                <span title={t(`species.taxonomy.labels.${rank.key}`)}
                  ><span class="sr-only"
                    >{t(`species.taxonomy.labels.${rank.key}`)}:
                  </span>{rank.value}</span
                >
              {/each}
            </span>
          {/if}
        </p>

        <div class="header-badges">
          <div role="group" aria-label={t('detections.detail.aria.classificationBadges')}>
            <VerificationBadges detection={det} size="sm" />
          </div>
          {#if det.source}
            <SourceBadge detection={det} variant="inline" />
          {/if}
          {#if det.isNewSpecies}
            <StatusPill variant="success" label={t('species.tracking.newSpecies')} size="sm" />
          {/if}
          {#if det.isNewThisYear}
            <StatusPill variant="success" label={t('species.tracking.newThisYear')} size="sm" />
          {/if}
          {#if det.isNewThisSeason && det.currentSeason}
            <StatusPill
              variant="info"
              label={`${t('species.tracking.newThisSeason')}: ${seasonLabel(det.currentSeason)}`}
              size="sm"
            />
          {/if}
          {#if hasSpeciesReferenceName(det.commonName)}
            <!-- Same pair offered on expanded detection rows. Both are addressed by
                 common name, so they are omitted when the detection has none rather
                 than linking to a guide page that does not exist. -->
            <span class="species-reference-links">
              <a
                href={getAllAboutBirdsUrl(det.commonName)}
                target="_blank"
                rel="noopener noreferrer"
                class="media-compare-link"
                aria-label={t('detections.detail.aria.viewOnAllAboutBirds', { name: displayName })}
              >
                <ExternalLink class="w-3.5 h-3.5" />
                <span>{t('detections.media.viewOnAllAboutBirds')}</span>
              </a>
              <a
                href={getWikipediaUrl(displayName, getLocale(), det.commonName)}
                target="_blank"
                rel="noopener noreferrer"
                class="media-compare-link"
                aria-label={t('detections.detail.aria.viewOnWikipedia', { name: displayName })}
              >
                <span class="species-reference-wikipedia-icon">W</span>
                <span>{t('detections.media.viewOnWikipedia')}</span>
              </a>
            </span>
          {/if}
        </div>
      </div>

      <div
        class="header-confidence"
        role="group"
        aria-label={t('detections.detail.aria.confidence', {
          confidence: Math.round((det.confidence ?? 0) * 100),
        })}
      >
        <ConfidenceCircle confidence={det.confidence} size="lg" />
      </div>

      <!-- Detection facts: one labelled row that wraps as the card narrows -->
      <dl class="detection-facts" aria-label={t('detections.detail.aria.metadata')}>
        <div class="fact">
          <dt class="fact-label">{t('detections.headers.dateTime')}</dt>
          <dd class="fact-value">
            <span>{det.date}</span>
            <span class="fact-time"><Clock class="w-3.5 h-3.5" />{det.time}</span>
            {#if det.timeOfDay}
              {@const tod = det.timeOfDay.toLowerCase()}
              <span class="time-of-day-badge tod-{tod}">
                {#if tod === 'day'}
                  <Sun size={12} />
                {:else if tod === 'night'}
                  <Moon size={12} />
                {:else if tod === 'sunrise'}
                  <Sunrise size={12} />
                {:else if tod === 'sunset'}
                  <Sunset size={12} />
                {/if}
                <span>{t(`detections.timeOfDay.${tod}`)}</span>
              </span>
            {/if}
          </dd>
        </div>

        {#if det.weather}
          <div class="fact">
            <dt class="fact-label">{t('detections.headers.weather')}</dt>
            <dd
              class="fact-value hero-weather"
              aria-label={t('detections.detail.aria.weatherConditions')}
            >
              <WeatherDetails
                weatherIcon={det.weather.weatherIcon}
                weatherDescription={det.weather.description}
                timeOfDay={det.timeOfDay?.toLowerCase() === 'night' ? 'night' : 'day'}
                temperature={det.weather.temperature}
                windSpeed={det.weather.windSpeed}
                windGust={det.weather.windGust}
                units={det.weather.units}
                size="md"
              />
            </dd>
          </div>
        {/if}

        {#if speciesInfo?.rarity}
          <div class="fact fact-rarity">
            <dt class="fact-label">{t('species.rarity.title')}</dt>
            <dd class="fact-value">
              <span class="rarity-label"
                >{t(`species.rarity.statuses.${speciesInfo.rarity.status}`)}</span
              >
              {#if speciesInfo.rarity.status !== 'unknown'}
                <span class="rarity-score">{(speciesInfo.rarity.score * 100).toFixed(0)}%</span>
              {/if}
            </dd>
            {#if speciesInfo.rarity.location_based && speciesInfo.rarity.latitude != null && speciesInfo.rarity.longitude != null}
              <dd class="fact-note">
                {t('species.rarity.basedOnLocation', {
                  latitude: speciesInfo.rarity.latitude.toFixed(2),
                  longitude: speciesInfo.rarity.longitude.toFixed(2),
                })}
              </dd>
            {/if}
          </div>
        {/if}

        {#if showDaysSinceFirst}
          <div class="fact">
            <dt class="fact-label">{t('species.tracking.daysSinceFirst')}</dt>
            <dd class="fact-value">{det.daysSinceFirstSeen}</dd>
          </div>
        {/if}
      </dl>

      {#if subspeciesList.length > 0}
        <details class="taxonomy-subspecies">
          <summary class="taxonomy-subspecies-summary">
            {t('species.taxonomy.subspecies')} ({subspeciesList.length})
          </summary>
          <div class="taxonomy-subspecies-list">
            {#each subspeciesList as subspecies, index (`${subspecies.scientific_name}_${index}`)}
              <div class="taxonomy-subspecies-item">
                <span class="italic">{subspecies.scientific_name}</span>
                {#if subspecies.common_name}
                  <span class="taxonomy-subspecies-common"
                    >{localizeSpeciesName(subspecies.scientific_name, subspecies.common_name)}</span
                  >
                {/if}
              </div>
            {/each}
          </div>
        </details>
      {/if}
    </div>
  </section>
{/snippet}

{#snippet detailTabs(det: Detection)}
  <section class="surface-card detail-tabs-card" aria-labelledby="tabs-heading">
    <h2 id="tabs-heading" class="sr-only">{t('detections.detail.aria.tabsHeading')}</h2>

    <!-- Tab Navigation -->
    <div class="tab-nav" role="tablist" aria-label={t('detections.detail.aria.tabList')}>
      {#each visibleTabs as tab (tab)}
        <button
          type="button"
          id="tab-{tab}"
          role="tab"
          class="tab-button"
          class:tab-active={activeTab === tab}
          aria-selected={activeTab === tab}
          aria-controls="tab-panel-{tab}"
          tabindex={activeTab === tab ? 0 : -1}
          onclick={() => (activeTab = tab)}
          onkeydown={handleTabKeydown}
        >
          {tab === 'review' ? t('common.actions.review') : t(`detections.tabs.${tab}`)}
        </button>
      {/each}
    </div>

    <!-- Tab Content -->
    <div class="tab-content">
      {#if activeTab === 'notes'}
        <div
          role="tabpanel"
          id="tab-panel-notes"
          aria-labelledby="tab-notes"
          aria-hidden="false"
          tabindex="0"
        >
          {@render notesTab(det)}
        </div>
      {:else if activeTab === 'history'}
        <div
          role="tabpanel"
          id="tab-panel-history"
          aria-labelledby="tab-history"
          aria-hidden="false"
          tabindex="0"
        >
          {@render historyTab(det)}
        </div>
      {:else if activeTab === 'review' && canReview && ReviewCard}
        <div
          role="tabpanel"
          id="tab-panel-review"
          aria-labelledby="tab-review"
          aria-hidden="false"
          tabindex="0"
        >
          <ReviewCard detection={det} onSaveComplete={handleReviewComplete} />
        </div>
      {/if}
    </div>
  </section>
{/snippet}

{#snippet historyTab(det: Detection)}
  <DetectionHistory scientificName={det.scientificName} />
{/snippet}

{#snippet notesTab(det: Detection)}
  <section aria-labelledby="notes-heading">
    <!-- The tab label already names this panel, so the heading is for screen readers only -->
    <h3 id="notes-heading" class="sr-only">{t('detections.notes.title')}</h3>
    {#if det.comments && det.comments.length > 0}
      <div class="space-y-3" role="list" aria-label={t('detections.detail.aria.comments')}>
        {#each det.comments as comment (comment.id ?? comment.createdAt)}
          <article class="content-panel" role="listitem">
            <p class="text-sm leading-relaxed">
              <span class="sr-only"
                >{t('detections.detail.aria.commentText')}:
              </span>{comment.entry}
            </p>
            <p class="text-xs text-[var(--color-base-content)]/40 mt-2">
              <span class="sr-only"
                >{t('detections.detail.aria.commentTimestamp')}:
              </span>{formatLocalDateTime(new Date(comment.createdAt))}
            </p>
          </article>
        {/each}
      </div>
    {:else}
      <div class="empty-state-section">
        <StickyNote class="empty-state-icon" />
        <p class="empty-state-text" role="status">
          {t('detections.notes.noComments')}
        </p>
      </div>
    {/if}
  </section>
{/snippet}

<!-- Main component -->
<main class="col-span-12 detection-detail" aria-label={t('detections.detail.aria.mainRegion')}>
  <!-- Loading state with live region -->
  <div role="status" aria-live="polite" class="sr-only">
    {#if isLoadingDetection}
      {t('detections.aria.loading')}
    {:else if detection}
      {t('detections.aria.loaded', {
        species: localizeSpeciesName(detection.scientificName, detection.commonName),
      })}
    {:else if detectionError}
      {t('detections.aria.error', { error: detectionError })}
    {/if}
  </div>

  {#if isLoadingDetection}
    <!-- Loading skeleton matching real layout structure (decorative, AT uses live region above) -->
    <div class="surface-card detection-header @container" aria-hidden="true">
      <div class="header-grid">
        <div class="hero-thumbnail skeleton-block"></div>
        <div class="header-identity skeleton-identity">
          <div class="skeleton-line w-56 h-7 mb-2"></div>
          <div class="skeleton-line w-72 h-4 mb-3"></div>
          <div class="flex gap-2">
            <div class="skeleton-line w-24 h-5 rounded-full"></div>
            <div class="skeleton-line w-28 h-5 rounded-full"></div>
          </div>
        </div>
        <div class="header-confidence">
          <div class="skeleton-block w-14 h-14 rounded-full"></div>
        </div>
        <div class="detection-facts">
          {#each Array(3) as _, i (i)}
            <div class="space-y-1.5">
              <div class="skeleton-line w-16 h-2.5"></div>
              <div class="skeleton-line w-32 h-4"></div>
            </div>
          {/each}
        </div>
      </div>
    </div>
    <!-- Media section skeleton -->
    <div class="surface-card media-card" aria-hidden="true">
      <div class="skeleton-line w-32 h-4 mb-3"></div>
      <div class="skeleton-block w-full" style:aspect-ratio="2 / 1"></div>
    </div>
  {:else if detectionError}
    <!-- Error state -->
    <div class="surface-card p-6">
      <div role="alert" aria-live="assertive">
        <ErrorAlert message={detectionError} />
      </div>
    </div>
  {:else if detection}
    {@render heroSection(detection)}

    <!-- Media Section (shown when this detection has a clip, or a kept spectrogram image
         after retention removed its audio) -->
    {#if detection.clipName || detection.spectrogramOnly}
      <section class="surface-card media-card" aria-labelledby="media-heading">
        <div class="media-heading-row">
          <h2 id="media-heading" class="section-heading !mb-0">
            {t('detections.media.title')}
          </h2>
          {#if hasSpeciesReferenceName(detection.commonName)}
            <a
              href={getAllAboutBirdsSoundsUrl(detection.commonName)}
              target="_blank"
              rel="noopener noreferrer"
              class="media-compare-link"
              aria-label={t('detections.detail.aria.compareSounds', {
                name: localizeSpeciesName(detection.scientificName, detection.commonName),
              })}
            >
              <ExternalLink class="w-3.5 h-3.5" />
              <span>{t('detections.media.compareSounds')}</span>
            </a>
          {/if}
          {#if clipExtractionEnabled && detection.clipName}
            <p class="media-hint">{t('detections.media.clipHint')}</p>
          {/if}
        </div>
        <div
          role="region"
          aria-label={detection.clipName
            ? t('detections.detail.aria.audioRecordingFor', {
                name: localizeSpeciesName(detection.scientificName, detection.commonName),
              })
            : t('components.audio.spectrogramForSpecies', {
                species: localizeSpeciesName(detection.scientificName, detection.commonName),
              })}
        >
          <div class="detail-audio-container">
            {#if detection.clipName}
              <AudioPlayer
                audioUrl={buildAppUrl(`/api/v2/audio/${detection.id}`)}
                detectionId={detection.id.toString()}
                showSpectrogram={true}
                spectrogramSize="lg"
                spectrogramRaw={false}
                responsive={true}
                className="w-full"
                enableClipExtraction={clipExtractionEnabled}
                clipLabel={`${detection.commonName}_${detection.date}_${detection.time.replace(/:/g, '-')}`}
                modelType={detection.modelType}
              />
            {:else}
              <SpectrogramImage
                detectionId={detection.id.toString()}
                size="lg"
                raw={false}
                className="w-full"
              />
            {/if}
          </div>
        </div>
      </section>
    {/if}

    {@render detailTabs(detection)}
  {/if}
</main>

<style>
  /* ===========================================
     DETECTION DETAIL - Editorial Design System
     =========================================== */

  .detection-detail {
    display: flex;
    flex-direction: column;
    gap: 1rem;
    width: 100%;
  }

  /* ----- Surface card (replaces DaisyUI card) ----- */
  .surface-card {
    background-color: var(--color-base-100);
    border-radius: var(--radius-box);
    border: 1px solid var(--border-100);
    min-width: 0;
  }

  /* ----- Header card ----- */
  .detection-header {
    padding: 1rem 1.25rem;
  }

  /* Narrow card: name and lineage beside the thumbnail, then badges and
     facts across the full width so the badges are not squeezed into a narrow
     column. Medium card: thumbnail, identity and confidence on one row, facts
     below at full width. Wide card: the facts move up beside the thumbnail, so
     the whole header is about as tall as the photo. */
  .header-grid {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto;
    grid-template-areas:
      'thumb name confidence'
      'thumb lineage lineage'
      'badges badges badges'
      'facts facts facts';
    align-items: start;
    gap: 0.375rem 0.75rem;
  }

  /* On a narrow card the identity block's children join the grid directly */
  .header-identity {
    display: contents;
  }

  .header-identity > .species-display-name {
    grid-area: name;
  }

  .header-identity > .species-lineage {
    grid-area: lineage;
  }

  .header-identity > .header-badges {
    grid-area: badges;
    margin-top: 0.25rem;
  }

  /* The loading skeleton's placeholder lines stay one block beside the thumbnail */
  @container (width < 30rem) {
    .header-identity.skeleton-identity {
      display: block;
      grid-row: 1 / span 2;
      grid-column: 2 / span 1;
    }
  }

  @container (min-width: 30rem) {
    .header-grid {
      grid-template-areas:
        'thumb identity confidence'
        'facts facts facts';
      gap: 0.75rem 1rem;
    }

    .header-identity {
      display: block;
      grid-area: identity;
      min-width: 0;
    }

    .header-identity > .header-badges {
      margin-top: 0.5rem;
    }
  }

  @container (min-width: 48rem) {
    .header-grid {
      grid-template-areas:
        'thumb identity confidence'
        'thumb facts facts';
      column-gap: 1.25rem;
    }
  }

  .header-confidence {
    grid-area: confidence;
  }

  /* ----- Reference links (media heading row styling is under .media-card) ----- */
  .media-compare-link {
    display: inline-flex;
    align-items: center;
    gap: 0.375rem;
    font-size: 0.8125rem;
    font-weight: 500;
    color: var(--color-base-content);
    opacity: 0.6;
    transition: opacity 0.15s ease;
    flex-shrink: 0;
  }

  .media-compare-link:hover {
    opacity: 1;
  }

  /* Reference links sit inline after the status badges in the header */
  .species-reference-links {
    display: inline-flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 0.75rem;
  }

  .species-reference-links .media-compare-link {
    text-decoration: none;
    white-space: nowrap;
  }

  .species-reference-links .media-compare-link:hover {
    text-decoration: underline;
  }

  .species-reference-wikipedia-icon {
    font-family: serif;
    font-weight: 700;
    font-size: 0.75rem;
    line-height: 1;
  }

  /* Species thumbnail - 4:3 to match avicommons 320×240 source images */
  .hero-thumbnail {
    grid-area: thumb;
    position: relative;
    width: 96px;
    aspect-ratio: 4 / 3;
    border-radius: 0.5rem;
    overflow: hidden;
    background: linear-gradient(135deg, var(--color-base-200), var(--color-base-300));
    box-shadow: var(--shadow-sm);
  }

  @container (min-width: 40rem) {
    .hero-thumbnail {
      width: 128px;
    }
  }

  @container (min-width: 48rem) {
    .hero-thumbnail {
      width: 160px;
    }
  }

  /* Photo credit - bottom-right corner of thumbnail */
  .thumbnail-credit {
    position: absolute;
    right: 0;
    bottom: 0;
    display: flex;
    align-items: center;
    gap: 0.2rem;
    padding: 0.2rem 0.4rem;
    background: oklch(10% 0 0deg / 0.55);
    border-top-left-radius: 0.375rem;
  }

  .thumbnail-credit :global(.credit-icon) {
    color: oklch(85% 0 0deg);
    flex-shrink: 0;
  }

  .credit-text {
    font-size: 0.5625rem;
    color: oklch(95% 0 0deg);
    line-height: 1;
    white-space: nowrap;
  }

  .credit-separator {
    font-size: 0.5625rem;
    color: oklch(70% 0 0deg);
    flex-shrink: 0;
    line-height: 1;
  }

  .credit-license {
    font-size: 0.5625rem;
    color: oklch(80% 0 0deg);
    text-decoration: none;
    line-height: 1;
    flex-shrink: 0;
    white-space: nowrap;
  }

  a.credit-license:hover {
    color: white;
    text-decoration: underline;
  }

  /* Species display name - refined Inter typography */
  .species-display-name {
    font-family: 'Inter Variable', Inter, ui-sans-serif, sans-serif;
    font-size: 1.25rem;
    font-weight: 650;
    line-height: 1.2;
    letter-spacing: -0.025em;
    color: var(--color-base-content);
    margin: 0;
    overflow-wrap: break-word;
  }

  @container (min-width: 40rem) {
    .species-display-name {
      font-size: 1.5rem;
    }
  }

  /* Scientific name followed by class › order › family on one wrapping line */
  .species-lineage {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0.125rem 0.75rem;
    margin-top: 0.125rem;
    font-size: 0.8125rem;
    color: var(--color-base-content);
  }

  .species-scientific-name {
    font-style: italic;
    opacity: 0.6;
    letter-spacing: 0.01em;
  }

  .taxonomy-path {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0 0.375rem;
    opacity: 0.55;
  }

  .taxonomy-sep {
    opacity: 0.6;
  }

  .header-badges {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.375rem;
  }

  /* ----- Detection facts: labelled values in one wrapping row ----- */
  .detection-facts {
    grid-area: facts;
    display: flex;
    flex-wrap: wrap;
    gap: 0.625rem 1.75rem;
    margin-top: 0.375rem;
    padding-top: 0.75rem;
    border-top: 1px solid var(--border-100);
    min-width: 0;
  }

  @container (min-width: 30rem) {
    .detection-facts {
      margin-top: 0;
    }
  }

  .fact {
    min-width: 0;
  }

  .fact-label {
    font-size: 0.625rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--color-base-content);
    opacity: 0.5;
    margin-bottom: 0.125rem;
  }

  .fact-value {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.25rem 0.5rem;
    font-size: 0.875rem;
    font-weight: 500;
    line-height: 1.5rem;
    color: var(--color-base-content);
  }

  .fact-time {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    opacity: 0.75;
  }

  .fact-note {
    font-size: 0.6875rem;
    color: var(--color-base-content);
    opacity: 0.5;
  }

  /* Time of day badge - theme-safe via alpha transparency */
  .time-of-day-badge {
    display: inline-flex;
    align-items: center;
    gap: 0.3rem;
    font-size: 0.6875rem;
    font-weight: 550;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    padding: 0.2rem 0.5rem;
    border-radius: 9999px;
    line-height: 1;
    color: var(--color-base-content);
  }

  .tod-day {
    background: oklch(80% 0.1 85deg / 0.4);
  }

  .tod-night {
    background: oklch(50% 0.08 270deg / 0.25);
  }

  .tod-sunrise {
    background: oklch(80% 0.12 55deg / 0.4);
  }

  .tod-sunset {
    background: oklch(78% 0.12 30deg / 0.4);
  }

  /* Weather on one line: condition, temperature, wind */
  .hero-weather :global(.wd-container) {
    flex-flow: row wrap;
    align-items: center;
    gap: 0.25rem 0.75rem;
  }

  .hero-weather :global(.wd-weather-row),
  .hero-weather :global(.wd-temperature-row),
  .hero-weather :global(.wd-wind-row) {
    gap: 0.25rem;
  }

  .hero-weather :global(.wd-weather-row span),
  .hero-weather :global(.wd-temperature-row span),
  .hero-weather :global(.wd-wind-row span) {
    font-size: 0.875rem;
  }

  .hero-weather :global(.wd-weather-icon) {
    font-size: 1rem;
  }

  .hero-weather :global(.wd-wind-row) {
    opacity: 0.6;
  }

  .rarity-label {
    font-weight: 600;
    text-transform: capitalize;
  }

  .rarity-score {
    font-size: 0.75rem;
    opacity: 0.6;
  }

  /* ----- Subspecies: collapsed under the facts ----- */
  .taxonomy-subspecies {
    grid-column: 1 / -1;
    min-width: 0;
  }

  @container (min-width: 48rem) {
    .taxonomy-subspecies {
      grid-column: 2 / -1;
    }
  }

  .taxonomy-subspecies-summary {
    font-size: 0.6875rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--color-base-content);
    opacity: 0.55;
    cursor: pointer;
    width: fit-content;
  }

  .taxonomy-subspecies-summary:focus-visible {
    outline: 2px solid var(--color-primary);
    outline-offset: 2px;
    border-radius: var(--radius-selector);
  }

  .taxonomy-subspecies-list {
    display: flex;
    flex-wrap: wrap;
    gap: 0.375rem;
    margin-top: 0.5rem;
  }

  .taxonomy-subspecies-item {
    display: flex;
    align-items: baseline;
    gap: 0.375rem;
    font-size: 0.8125rem;
    color: var(--color-base-content);
    padding: 0.125rem 0.5rem;
    background-color: var(--color-base-200);
    border: 1px solid var(--border-100);
    border-radius: var(--radius-field);
  }

  .taxonomy-subspecies-common {
    font-size: 0.75rem;
    opacity: 0.5;
  }

  /* ----- Section Heading ----- */
  .section-heading {
    font-size: 0.8125rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    color: var(--color-base-content);
    opacity: 0.65;
    margin-bottom: 0.625rem;
  }

  /* ----- Content Panel (inner blocks) ----- */
  .content-panel {
    background-color: var(--color-base-200);
    border-radius: var(--radius-field);
    border: 1px solid var(--border-100);
    padding: 0.75rem;
  }

  /* ----- Media card: heading and hint share one line when they fit ----- */
  .media-card {
    padding: 1rem 1.25rem 1.25rem;
  }

  .media-heading-row {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0.125rem 0.75rem;
    margin-bottom: 0.75rem;
  }

  .media-heading-row .media-compare-link {
    margin-left: auto;
  }

  .media-hint {
    font-size: 0.8125rem;
    color: var(--color-base-content);
    opacity: 0.6;
  }

  .detail-audio-container {
    width: 100%;
  }

  .detail-audio-container :global(.group) {
    position: relative;
  }

  .detail-audio-container :global(img) {
    width: 100%;
    height: auto;
    display: block;
  }

  /* ----- Tabs card: notes, history and review below the recording ----- */
  .detail-tabs-card {
    padding: 0.25rem 1.25rem 1.25rem;
  }

  .tab-content {
    margin-top: 1rem;
  }

  .tab-nav {
    display: flex;
    gap: 0;
    border-bottom: 1px solid var(--border-100);
    overflow-x: auto;
    -webkit-overflow-scrolling: touch;
    scrollbar-width: none;
  }

  .tab-nav::-webkit-scrollbar {
    display: none;
  }

  .tab-button {
    position: relative;
    padding: 0.625rem 0.875rem;
    font-size: 0.8125rem;
    font-weight: 500;
    color: var(--color-base-content);
    opacity: 0.55;
    background: none;
    border: none;
    cursor: pointer;
    white-space: nowrap;
    transition:
      opacity 0.15s ease,
      color 0.15s ease;
    border-bottom: 2px solid transparent;
    margin-bottom: -1px;
  }

  .tab-button:hover {
    opacity: 0.85;
  }

  .tab-button:focus-visible {
    outline: 2px solid var(--color-primary);
    outline-offset: -2px;
    border-radius: var(--radius-selector);
  }

  .tab-button.tab-active {
    opacity: 1;
    color: var(--color-primary);
    border-bottom-color: var(--color-primary);
    font-weight: 600;
  }

  /* ----- Empty States ----- */
  /* One compact line: icon, optional heading, message */
  .empty-state-section {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.25rem 0.5rem;
    padding: 0.25rem 0;
  }

  .empty-state-section :global(.empty-state-icon) {
    width: 1rem;
    height: 1rem;
    color: var(--color-base-content);
    opacity: 0.3;
    flex-shrink: 0;
  }

  .empty-state-heading {
    font-size: 0.875rem;
    font-weight: 600;
    color: var(--color-base-content);
    opacity: 0.55;
  }

  .empty-state-text {
    font-size: 0.875rem;
    color: var(--color-base-content);
    opacity: 0.4;
    font-style: italic;
  }

  /* ----- Skeleton loading primitives ----- */
  .skeleton-line,
  .skeleton-block {
    background: var(--color-base-300);
    border-radius: var(--radius-selector);
    animation: skeleton-pulse 1.5s ease-in-out infinite;
  }

  .skeleton-block {
    border-radius: var(--radius-box);
  }

  @keyframes skeleton-pulse {
    0%,
    100% {
      opacity: 1;
    }

    50% {
      opacity: 0.5;
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .skeleton-line,
    .skeleton-block {
      animation: none;
    }
  }
</style>
