package datastore

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/birdnet-go/internal/suncalc"
)

// todSampleStep spaces the detections sampled around each sun event.
const todSampleStep = 5 * time.Minute

// todFullYearDays spans the longest date range the time-of-day filter accepts
// without tripping the 365-day guard across a DST transition.
const todFullYearDays = maxTimeOfDayFilterDays - 1

// newSunAdvancedSearchStore returns an in-memory legacy store with a SunCalc
// for the given station, so the advanced time-of-day filter resolves real sun
// events instead of falling back to clock windows.
func newSunAdvancedSearchStore(tb testing.TB, latitude, longitude float64) *DataStore {
	tb.Helper()
	db := openSQLiteTestDB(tb)
	require.NoError(tb, db.AutoMigrate(&Note{}, &NoteReview{}, &NoteLock{}, &NoteComment{}))
	return &DataStore{DB: db, SunCalc: suncalc.NewSunCalc(latitude, longitude)}
}

// expectedTimeOfDay labels a stored note the way SearchDetections does: against
// the sun events of its own date, via the shared per-row classifier.
func expectedTimeOfDay(t *testing.T, ds *DataStore, note *Note) string {
	t.Helper()
	sun, err := ds.getSunEventsForDate(note.Date)
	require.NoError(t, err)
	ts, err := time.ParseInLocation(time.DateTime, note.Date+" "+note.Time, time.Local)
	require.NoError(t, err)
	return suncalc.ClassifyTimeOfDay(ts, &sun)
}

// TestSearchNotesAdvanced_TimeOfDayMatchesClassifier checks the advanced search
// time-of-day filter against the per-row classifier on real sun events, for
// single periods, multiple periods, the legacy aliases and a paginated page, and
// that the reported total always equals the number of matching rows.
//
// Detections are sampled every few minutes across each sun event on three
// consecutive dates, where a filter bound off by even one sample changes the
// answer. Both stations sit far from any plausible server timezone, so at least
// one of them exercises a large server/station offset on any machine.
func TestSearchNotesAdvanced_TimeOfDayMatchesClassifier(t *testing.T) {
	stations := []struct {
		name      string
		latitude  float64
		longitude float64
	}{
		{"Honolulu (UTC-10)", 21.3069, -157.8583},
		{"Auckland (UTC+13)", -36.8485, 174.7633},
	}
	targetDate := time.Date(2025, 3, 20, 0, 0, 0, 0, time.UTC)

	for _, station := range stations {
		t.Run(station.name, func(t *testing.T) {
			ds := newSunAdvancedSearchStore(t, station.latitude, station.longitude)
			stationZone, err := time.LoadLocation(ds.SunCalc.LocationName())
			require.NoError(t, err)

			sampleSpan := suncalc.SunEventWindow + 2*todSampleStep
			var notes []Note
			for dayOffset := -1; dayOffset <= 1; dayOffset++ {
				noon := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day()+dayOffset,
					12, 0, 0, 0, stationZone)
				sun, err := ds.SunCalc.GetSunEventTimes(noon)
				require.NoError(t, err)
				samples := []time.Time{noon, noon.Add(12 * time.Hour)}
				for _, event := range []time.Time{sun.Sunrise, sun.Sunset} {
					for offset := -sampleSpan; offset <= sampleSpan; offset += todSampleStep {
						samples = append(samples, event.Add(offset))
					}
				}
				for _, s := range samples {
					local := s.In(time.Local)
					notes = append(notes, Note{Date: local.Format(time.DateOnly), Time: local.Format(time.TimeOnly),
						ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.9})
				}
			}
			require.NoError(t, ds.DB.Create(&notes).Error)

			byPeriod := make(map[string][]uint)
			minDate, maxDate := notes[0].Date, notes[0].Date
			for i := range notes {
				label := expectedTimeOfDay(t, ds, &notes[i])
				byPeriod[label] = append(byPeriod[label], notes[i].ID)
				minDate = min(minDate, notes[i].Date)
				maxDate = max(maxDate, notes[i].Date)
			}
			for _, p := range allTimeOfDayPeriods {
				require.NotEmptyf(t, byPeriod[p], "fixture must populate %q", p)
			}

			start, err := time.ParseInLocation(time.DateOnly, minDate, time.Local)
			require.NoError(t, err)
			end, err := time.ParseInLocation(time.DateOnly, maxDate, time.Local)
			require.NoError(t, err)

			cases := []struct {
				name    string
				periods []string
				want    []string
			}{
				{"sunrise", []string{TimeOfDaySunrise}, []string{TimeOfDaySunrise}},
				{"sunset", []string{TimeOfDaySunset}, []string{TimeOfDaySunset}},
				{"day", []string{TimeOfDayDay}, []string{TimeOfDayDay}},
				{"night", []string{TimeOfDayNight}, []string{TimeOfDayNight}},
				{"legacy aliases", []string{"dawn", "dusk"}, []string{TimeOfDaySunrise, TimeOfDaySunset}},
				{"day and night", []string{TimeOfDayDay, TimeOfDayNight}, []string{TimeOfDayDay, TimeOfDayNight}},
				{"all periods", allTimeOfDayPeriods, allTimeOfDayPeriods},
			}
			ranges := []struct {
				name  string
				start time.Time
			}{
				{"sample dates", start},
				// A full year of derived-table rows, the largest the filter builds.
				{"full year", end.AddDate(0, 0, -todFullYearDays)},
			}
			for _, r := range ranges {
				for _, tc := range cases {
					t.Run(r.name+"/"+tc.name, func(t *testing.T) {
						var want []uint
						for _, p := range tc.want {
							want = append(want, byPeriod[p]...)
						}
						got, total, err := ds.SearchNotesAdvanced(&AdvancedSearchFilters{
							TimeOfDay: tc.periods,
							DateRange: &DateRange{Start: r.start, End: end},
						})
						require.NoError(t, err)
						assert.ElementsMatch(t, want, resultIDs(got))
						assert.Equal(t, int64(len(want)), total, "COUNT must equal the matching row count")
					})
				}
			}

			t.Run("paginated total counts every match", func(t *testing.T) {
				const pageSize = 3
				want := byPeriod[TimeOfDayNight]
				require.Greater(t, len(want), pageSize)
				got, total, err := ds.SearchNotesAdvanced(&AdvancedSearchFilters{
					TimeOfDay: []string{TimeOfDayNight},
					DateRange: &DateRange{Start: start, End: end},
					Limit:     pageSize,
					Offset:    1,
				})
				require.NoError(t, err)
				assert.Len(t, got, pageSize)
				assert.Subset(t, want, resultIDs(got))
				assert.Equal(t, int64(len(want)), total)
			})

			t.Run("simple search agrees", func(t *testing.T) {
				for _, p := range allTimeOfDayPeriods {
					results, total, err := ds.SearchDetections(&SearchFilters{
						TimeOfDay: p,
						DateStart: minDate,
						DateEnd:   maxDate,
						PerPage:   len(notes) + 1,
					})
					require.NoError(t, err)
					assert.Equal(t, len(byPeriod[p]), total, "simple search COUNT for %q", p)
					got := make([]uint, 0, len(results))
					for i := range results {
						assert.Equal(t, p, results[i].TimeOfDay)
						id, parseErr := strconv.ParseUint(results[i].ID, 10, 64)
						require.NoError(t, parseErr)
						got = append(got, uint(id))
					}
					assert.ElementsMatch(t, byPeriod[p], got, "simple search rows for %q", p)
				}
			})
		})
	}
}

// TestSearchNotesAdvanced_TimeOfDayDefaultsToLastYear covers an advanced query
// with no date range: the filter resolves sun events over the last year and
// still classifies today's detections exactly like the per-row classifier.
func TestSearchNotesAdvanced_TimeOfDayDefaultsToLastYear(t *testing.T) {
	ds := newSunAdvancedSearchStore(t, 60.1699, 24.9384)
	today := time.Now().Format(time.DateOnly)

	const samplesPerHour = 4
	notes := make([]Note, 0, 24*samplesPerHour)
	for i := range 24 * samplesPerHour {
		clock := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour / samplesPerHour)
		notes = append(notes, Note{Date: today, Time: clock.Format(time.TimeOnly),
			ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.9})
	}
	require.NoError(t, ds.DB.Create(&notes).Error)

	byPeriod := make(map[string][]uint)
	for i := range notes {
		label := expectedTimeOfDay(t, ds, &notes[i])
		byPeriod[label] = append(byPeriod[label], notes[i].ID)
	}

	total := 0
	for _, p := range allTimeOfDayPeriods {
		got, count, err := ds.SearchNotesAdvanced(&AdvancedSearchFilters{TimeOfDay: []string{p}})
		require.NoError(t, err)
		assert.ElementsMatch(t, byPeriod[p], resultIDs(got), "period %q", p)
		assert.Equal(t, int64(len(byPeriod[p])), count, "period %q", p)
		total += len(got)
	}
	assert.Len(t, notes, total, "the four periods must partition today's detections")
}

// BenchmarkSearchNotesAdvanced_TimeOfDayFullYear measures advanced time-of-day
// searches over a full year of detections, the case that previously OR-ed one
// clause per date and period and evaluated it twice (COUNT and page query).
func BenchmarkSearchNotesAdvanced_TimeOfDayFullYear(b *testing.B) {
	const (
		notesPerDay = 48
		pageSize    = 50
	)
	ds := newSunAdvancedSearchStore(b, 60.1699, 24.9384)
	end := time.Date(2025, 12, 31, 0, 0, 0, 0, time.Local)
	start := end.AddDate(0, 0, -todFullYearDays)

	notes := make([]Note, 0, (todFullYearDays+1)*notesPerDay)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		for i := range notesPerDay {
			clock := d.Add(time.Duration(i) * 24 * time.Hour / notesPerDay)
			notes = append(notes, Note{Date: d.Format(time.DateOnly), Time: clock.Format(time.TimeOnly),
				ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.9})
		}
	}
	require.NoError(b, ds.DB.CreateInBatches(&notes, 500).Error)

	periodSets := []struct {
		name    string
		periods []string
	}{
		{"night", []string{TimeOfDayNight}},
		{"sunrise+sunset", []string{TimeOfDaySunrise, TimeOfDaySunset}},
		{"all", allTimeOfDayPeriods},
	}
	for _, ps := range periodSets {
		b.Run(ps.name, func(b *testing.B) {
			filters := &AdvancedSearchFilters{
				TimeOfDay: ps.periods,
				DateRange: &DateRange{Start: start, End: end},
				Limit:     pageSize,
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_, _, err := ds.SearchNotesAdvanced(filters)
				require.NoError(b, err)
			}
		})
	}
}
