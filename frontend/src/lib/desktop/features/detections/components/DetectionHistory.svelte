<!--
  DetectionHistory: the "History" tab of the detection detail view.

  Shows the species' activity history by reusing the analytics hub's Seasonal Activity Heatmap
  card (the same registry entry, so loading/empty/error states and the chart stay identical),
  scoped to this detection's species over a rolling window the user can switch.
-->
<script lang="ts">
  import { t } from '$lib/i18n';
  import ChartCard from '$lib/desktop/features/analytics/components/ChartCard.svelte';
  import { CHART_REGISTRY } from '$lib/desktop/features/analytics/registry/charts';
  import {
    resolveDateRange,
    formatDateForAPI,
  } from '$lib/desktop/features/analytics/registry/analyticsParams';
  import type {
    AnalyticsParams,
    DateRangePreset,
  } from '$lib/desktop/features/analytics/registry/types';

  interface Props {
    /** Scientific name of the detection's species. */
    scientificName: string;
  }

  let { scientificName }: Props = $props();

  const HEATMAP_CHART_ID = 'seasonal-heatmap';
  type HistoryRange = Extract<DateRangePreset, 'quarter' | 'year'>;
  const RANGES: readonly HistoryRange[] = ['quarter', 'year'];
  const DEFAULT_HISTORY_RANGE: HistoryRange = 'year';

  const heatmapChart = CHART_REGISTRY.find(chart => chart.id === HEATMAP_CHART_ID);

  let range = $state<HistoryRange>(DEFAULT_HISTORY_RANGE);

  const params = $derived.by<AnalyticsParams>(() => {
    const [startDate, endDate] = resolveDateRange(range, '', '');
    return {
      range,
      start: formatDateForAPI(startDate),
      end: formatDateForAPI(endDate),
      species: [scientificName],
      source: '',
      startDate,
      endDate,
    };
  });
</script>

{#if heatmapChart}
  <section class="space-y-4" aria-labelledby="history-heading">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h3 id="history-heading" class="sr-only">{t('detections.history.title')}</h3>
      <div
        class="inline-flex rounded-lg border border-[var(--color-base-200)] overflow-hidden"
        role="group"
        aria-label={t('analytics.advanced.dateRange')}
      >
        {#each RANGES as option (option)}
          <button
            type="button"
            class="px-3 py-1.5 text-sm font-medium transition-colors"
            class:bg-primary={range === option}
            class:text-primary-content={range === option}
            aria-pressed={range === option}
            onclick={() => (range = option)}
          >
            {t(`analytics.advanced.dateRangeOptions.${option}`)}
          </button>
        {/each}
      </div>
    </div>

    <ChartCard chart={heatmapChart} {params} />
  </section>
{/if}
