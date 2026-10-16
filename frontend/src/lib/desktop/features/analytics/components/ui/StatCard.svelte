<script lang="ts">
  import { cn } from '$lib/utils/cn';
  import type { Snippet } from 'svelte';

  interface Props {
    title: string;
    value: string | number;
    subtitle?: string;
    icon?: Snippet;
    iconClassName?: string;
    className?: string;
    valueClassName?: string;
    isLoading?: boolean;
  }

  let {
    title,
    value,
    subtitle,
    icon,
    iconClassName = '',
    className = '',
    valueClassName = '',
    isLoading = false,
  }: Props = $props();

  // A truncated value is unreadable once it overflows (a long species name becomes
  // "Jumping Bush ..."), so expose the full text as a native tooltip. Values that are
  // not truncated get no title: a tooltip repeating text already fully on screen is
  // just noise. Keyed off the caller's own truncate class, so any card that starts
  // truncating picks this up without a second prop to keep in sync.
  let valueTitle = $derived(
    valueClassName.includes('truncate') && !isLoading ? String(value) : undefined
  );
</script>

<div class={cn('card bg-[var(--color-base-100)] shadow-xs', className)}>
  <div class="card-body p-4 md:p-6">
    <h2 class="card-title text-lg">{title}</h2>
    <div class="flex items-center gap-3">
      {#if icon}
        <div class={cn('w-12 h-12 rounded-full flex items-center justify-center', iconClassName)}>
          {@render icon()}
        </div>
      {/if}
      <div>
        <div class={cn('text-3xl font-bold', valueClassName)} title={valueTitle}>
          {isLoading ? '...' : value}
        </div>
        {#if subtitle}
          <div class="text-xs text-[var(--color-base-content)] opacity-60">{subtitle}</div>
        {/if}
      </div>
    </div>
  </div>
</div>
