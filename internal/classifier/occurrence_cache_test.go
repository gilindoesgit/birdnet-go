package classifier

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/birdnet-go/internal/conf"
	"github.com/tphakala/birdnet-go/internal/conf/conftest"
)

// occurrenceCacheTestDelta is the float tolerance for occurrence comparisons.
const occurrenceCacheTestDelta = 0.001

// benchOccurrenceSpecies approximates the size of a real geomodel label space, so the
// benchmark index (raw plus canonical keys) is the same order of magnitude as in
// production.
const benchOccurrenceSpecies = 6500

// newOccurrenceCacheSettings returns a location-configured settings snapshot whose
// classifier labels are labels.
func newOccurrenceCacheSettings(tb testing.TB, labels []string) *conf.Settings {
	tb.Helper()
	settings := conftest.GetTestSettings()
	settings.BirdNET.Latitude = 60.0
	settings.BirdNET.Longitude = 25.0
	settings.BirdNET.LocationConfigured = true
	settings.BirdNET.RangeFilter.Threshold = 0.01
	settings.BirdNET.Labels = labels
	return settings
}

// newScoredBackend returns a fake universal range filter that reports scores.
func newScoredBackend(scores []SpeciesScore) *fakeUniversalRangeFilter {
	labels := make([]string, 0, len(scores))
	raw := make([]float32, 0, len(scores))
	for _, s := range scores {
		labels = append(labels, s.Label)
		raw = append(raw, float32(s.Score))
	}
	return &fakeUniversalRangeFilter{geoLabels: labels, scores: scores, rawScores: raw}
}

// TestCachedOccurrence_MatchesCopyingPath verifies the in-place lookup answers exactly
// as the copying path plus lookupOccurrence did, on both the miss and the hit path,
// for exact (raw) keys, alias (canonical) keys, colliding species and absent species.
func TestCachedOccurrence_MatchesCopyingPath(t *testing.T) {
	scores := []SpeciesScore{
		{Label: collidingLabelA, Score: 0.9},
		{Label: "Turdus merula_Common Blackbird", Score: 0.7},
		{Label: aliasCanonicalLabel, Score: 0.4},
		{Label: collidingLabelB, Score: 0.1},
	}
	labels := []string{collidingLabelA, "Turdus merula_Common Blackbird", aliasCanonicalLabel, collidingLabelB}
	settings := newOccurrenceCacheSettings(t, labels)
	date := time.Now()

	tests := []struct {
		name      string
		species   string
		wantScore float64
		wantFound bool
	}{
		{name: "raw key", species: "Turdus merula_Common Blackbird", wantScore: 0.7, wantFound: true},
		{name: "canonical key via legacy synonym", species: aliasLegacyLabel, wantScore: 0.4, wantFound: true},
		{name: "colliding species A keeps its own score", species: collidingLabelA, wantScore: 0.9, wantFound: true},
		{name: "colliding species B keeps its own score", species: collidingLabelB, wantScore: 0.1, wantFound: true},
		{name: "absent species", species: "Myotis brandtii", wantFound: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rfs := newTestRangeFilterService(newScoredBackend(scores))

			// Cold cache: the miss path computes, stores and answers.
			missScore, missFound, err := rfs.cachedOccurrence(date, tt.species, settings)
			require.NoError(t, err)
			assert.Equal(t, 1, rfs.speciesCacheLen(), "miss path should populate the cache")

			// Warm cache: the hit path answers from the shared index.
			hitScore, hitFound, err := rfs.cachedOccurrence(date, tt.species, settings)
			require.NoError(t, err)

			// Reference: the copying path the per-detection lookup used before.
			copied, err := rfs.getCachedSpeciesScores(date, settings)
			require.NoError(t, err)
			refScore, refFound := lookupOccurrence(copied, tt.species)

			assert.Equal(t, tt.wantFound, refFound, "reference lookup")
			assert.Equal(t, refFound, missFound, "miss path found")
			assert.Equal(t, refFound, hitFound, "hit path found")
			if tt.wantFound {
				assert.InDelta(t, tt.wantScore, refScore, occurrenceCacheTestDelta)
				assert.InDelta(t, refScore, missScore, occurrenceCacheTestDelta)
				assert.InDelta(t, refScore, hitScore, occurrenceCacheTestDelta)
			}
		})
	}
}

// TestGetCachedSpeciesScores_ReturnsPrivateCopy verifies getCachedSpeciesScores still
// hands its caller a map it owns: mutating it must not reach the published entry
// that cachedOccurrence reads in place.
func TestGetCachedSpeciesScores_ReturnsPrivateCopy(t *testing.T) {
	const label = "Turdus merula_Common Blackbird"
	settings := newOccurrenceCacheSettings(t, []string{label})
	rfs := newTestRangeFilterService(newScoredBackend([]SpeciesScore{{Label: label, Score: 0.7}}))
	date := time.Now()

	copied, err := rfs.getCachedSpeciesScores(date, settings)
	require.NoError(t, err)
	for k := range copied {
		copied[k] = 0.0
	}

	got, found, err := rfs.cachedOccurrence(date, label, settings)
	require.NoError(t, err)
	require.True(t, found)
	assert.InDelta(t, 0.7, got, occurrenceCacheTestDelta, "published entry must be unaffected by the caller's copy")
}

// TestCachedOccurrence_GenerationMismatchNotServed verifies the in-place lookup keeps
// the generation gate: swapTestBackend bumps the generation without clearing the
// cache, so a stale entry survives in the map and only the gate keeps it from being
// served.
func TestCachedOccurrence_GenerationMismatchNotServed(t *testing.T) {
	const label = "Turdus merula_Common Blackbird"
	settings := newOccurrenceCacheSettings(t, []string{label})
	rfs := newTestRangeFilterService(newScoredBackend([]SpeciesScore{{Label: label, Score: 0.9}}))
	date := time.Now()

	got, found, err := rfs.cachedOccurrence(date, label, settings)
	require.NoError(t, err)
	require.True(t, found)
	assert.InDelta(t, 0.9, got, occurrenceCacheTestDelta)
	require.Equal(t, 1, rfs.speciesCacheLen())

	rfs.swapTestBackend(newScoredBackend([]SpeciesScore{{Label: label, Score: 0.1}}))

	got, found, err = rfs.cachedOccurrence(date, label, settings)
	require.NoError(t, err)
	require.True(t, found)
	assert.InDelta(t, 0.1, got, occurrenceCacheTestDelta, "entry from the superseded generation must not be served")

	// The re-computed entry is cached at the new generation and served on the next read.
	got, found, err = rfs.cachedOccurrence(date, label, settings)
	require.NoError(t, err)
	require.True(t, found)
	assert.InDelta(t, 0.1, got, occurrenceCacheTestDelta)
	assert.Equal(t, 1, rfs.speciesCacheLen(), "new generation replaces the stale entry under the same key")
}

// TestCachedOccurrence_SkipsCacheWhenReloadRacesPrediction verifies the shared miss
// path keeps the write-path anti-poisoning guard for the in-place lookup: scores
// computed across a mid-prediction reload answer this caller but are not cached.
func TestCachedOccurrence_SkipsCacheWhenReloadRacesPrediction(t *testing.T) {
	const label = "Turdus merula_Common Blackbird"
	settings := newOccurrenceCacheSettings(t, []string{label})
	backend := &fakeReloadingRangeFilter{fakeUniversalRangeFilter: *newScoredBackend([]SpeciesScore{{Label: label, Score: 0.7}})}
	rfs := newTestRangeFilterService(backend)
	backend.rfs = rfs
	date := time.Now()

	got, found, err := rfs.cachedOccurrence(date, label, settings)
	require.NoError(t, err)
	require.True(t, found)
	assert.InDelta(t, 0.7, got, occurrenceCacheTestDelta)
	assert.Zero(t, rfs.speciesCacheLen(), "scores computed across a mid-prediction reload must not be cached")

	_, _, err = rfs.cachedOccurrence(date, label, settings)
	require.NoError(t, err)
	assert.Equal(t, 1, rfs.speciesCacheLen(), "an uncontended read should cache normally")
}

// TestCachedOccurrence_ConcurrentReads runs in-place lookups, copying reads and
// backend swaps concurrently under the race detector, proving the shared index is
// only ever read after publication and that no reader sees an out-of-range score.
func TestCachedOccurrence_ConcurrentReads(t *testing.T) {
	const (
		label      = "Turdus merula_Common Blackbird"
		readers    = 8
		iterations = 200
		swaps      = 100
		scoreSteps = 10
	)
	settings := newOccurrenceCacheSettings(t, []string{label})
	newBackend := func(score float64) *fakeUniversalRangeFilter {
		return newScoredBackend([]SpeciesScore{{Label: label, Score: score}})
	}
	rfs := newTestRangeFilterService(newBackend(0.5))
	date := time.Now()

	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			for range iterations {
				got, found, err := rfs.cachedOccurrence(date, label, settings)
				if err != nil {
					t.Errorf("cachedOccurrence: %v", err)
					return
				}
				if !found || got < 0 || got > 1 {
					t.Errorf("unexpected occurrence %v (found=%v)", got, found)
					return
				}
			}
		})
		wg.Go(func() {
			for range iterations {
				if _, err := rfs.getCachedSpeciesScores(date, settings); err != nil {
					t.Errorf("getCachedSpeciesScores: %v", err)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for i := range swaps {
			rfs.swapTestBackend(newBackend(float64(i%scoreSteps) / scoreSteps))
		}
	})
	wg.Wait()

	require.Positive(t, rfs.loadState().generation, "concurrent swaps should have advanced the generation")
}

// BenchmarkOccurrenceLookup compares the per-detection occurrence lookup on a warm
// cache: copying the whole index then looking one species up (the previous path)
// against looking it up in place.
func BenchmarkOccurrenceLookup(b *testing.B) {
	scores := make([]SpeciesScore, 0, benchOccurrenceSpecies)
	labels := make([]string, 0, benchOccurrenceSpecies)
	for i := range benchOccurrenceSpecies {
		label := fmt.Sprintf("Genus%04d species%04d_Common Bird %d", i, i, i)
		labels = append(labels, label)
		scores = append(scores, SpeciesScore{Label: label, Score: 0.5})
	}
	settings := newOccurrenceCacheSettings(b, labels)
	rfs := newTestRangeFilterService(newScoredBackend(scores))
	date := time.Now()
	species := labels[benchOccurrenceSpecies/2]

	// Warm the cache so both sub-benchmarks measure the hit path.
	_, found, err := rfs.cachedOccurrence(date, species, settings)
	require.NoError(b, err)
	require.True(b, found)

	b.Run("copy_then_lookup", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			copied, err := rfs.getCachedSpeciesScores(date, settings)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = lookupOccurrence(copied, species)
		}
	})

	b.Run("in_place", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, _, err := rfs.cachedOccurrence(date, species, settings); err != nil {
				b.Fatal(err)
			}
		}
	})
}
