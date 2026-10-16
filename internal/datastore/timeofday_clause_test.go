package datastore

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/birdnet-go/internal/suncalc"
	"gorm.io/gorm"
)

// clauseTestWindow tracks the production window (suncalc.SunEventWindow), the
// same value the SQL builder and the per-row classifier use, so the sweep cannot
// silently pass against a window that has drifted from production.
const clauseTestWindow = suncalc.SunEventWindow

// clauseTestDate is the single calendar date the classifier sweep seeds.
const clauseTestDate = "2025-07-15"

// minutesPerDay is the number of one-minute samples the sweep inserts.
const minutesPerDay = 24 * 60

// allTimeOfDayPeriods lists the four canonical periods in classifier order.
var allTimeOfDayPeriods = []string{TimeOfDaySunrise, TimeOfDaySunset, TimeOfDayDay, TimeOfDayNight}

// boundsFor builds one date's timeOfDayBounds from its sunrise and sunset, the
// same way resolveTimeOfDayBounds does.
func boundsFor(date string, sun *suncalc.SunEventTimes) timeOfDayBounds {
	return timeOfDayBounds{
		date:         date,
		sunrise:      sun.Sunrise.Format(time.TimeOnly),
		sunset:       sun.Sunset.Format(time.TimeOnly),
		sunriseStart: sun.Sunrise.Add(-clauseTestWindow).Format(time.TimeOnly),
		sunriseEnd:   sun.Sunrise.Add(clauseTestWindow).Format(time.TimeOnly),
		sunsetStart:  sun.Sunset.Add(-clauseTestWindow).Format(time.TimeOnly),
		sunsetEnd:    sun.Sunset.Add(clauseTestWindow).Format(time.TimeOnly),
	}
}

// sunFilteredNoteIDs runs applyTimeOfDaySunFilter over the notes table and
// returns the matching IDs, asserting that COUNT agrees with the row query.
func sunFilteredNoteIDs(t *testing.T, db *gorm.DB, periods []string, bounds []timeOfDayBounds) []uint {
	t.Helper()
	query, err := applyTimeOfDaySunFilter(db.Model(&Note{}), periods, bounds)
	require.NoError(t, err)

	var count int64
	require.NoError(t, query.Session(&gorm.Session{}).Count(&count).Error)

	var notes []Note
	require.NoError(t, query.Order("notes.id").Find(&notes).Error)
	assert.Equal(t, int64(len(notes)), count, "COUNT must agree with the row query for %v", periods)
	return resultIDs(notes)
}

// TestTimeOfDaySunFilter_MatchesClassifier proves the SQL time-of-day filter
// selects exactly the same category as the per-row classifier
// suncalc.ClassifyTimeOfDay, for every clock minute across 24 hours, in several
// sun-event scenarios. The predicates run in SQLite, so the wraparound decisions
// the database makes per row are what is checked. The high-latitude scenarios
// exercise sunrise/sunset windows (and the lit span) that cross midnight.
func TestTimeOfDaySunFilter_MatchesClassifier(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	base := time.Date(2025, 7, 15, 0, 0, 0, 0, loc)

	scenarios := []struct {
		name    string
		sunrise time.Duration
		sunset  time.Duration
	}{
		{"normal mid-latitude day", 6 * time.Hour, 21 * time.Hour},
		{"high-latitude sunset near midnight", 3 * time.Hour, 23*time.Hour + 50*time.Minute},
		{"high-latitude sunrise near midnight", 15 * time.Minute, 22 * time.Hour},
		{"short winter day", 9 * time.Hour, 15 * time.Hour},
		// Sub-60-minute day (near-total-night winter): the sunrise and sunset
		// windows overlap, exercising the sunset predicate's sunrise-window
		// exclusion and the empty "day" set.
		{"sub-60min day, overlapping windows", 12 * time.Hour, 12*time.Hour + 40*time.Minute},
		// Just over 60-minute day: windows are disjoint but the "day" span is a
		// single minute wide.
		{"very short day, disjoint windows", 12 * time.Hour, 13*time.Hour + 1*time.Minute},
		// Sub-60-minute night (polar summer, day > 23h): the sunrise and sunset
		// windows overlap across midnight, so the night set is empty.
		{"sub-60min night, overlapping windows", 10 * time.Minute, 23*time.Hour + 50*time.Minute},
		// Summer inversion: local sunset falls after midnight (e.g. Reykjavik/Oslo
		// ~65N near the solstice), so the daytime arc wraps midnight and sunset's
		// wall clock precedes sunrise's. Midday must be classified as day.
		{"summer inversion, sunset after midnight", 2*time.Hour + 2*time.Minute, 1 * time.Minute},
		// More extreme inversion with a ~1h night: sunrise 02:00, sunset 01:00.
		{"summer inversion, short night", 2 * time.Hour, 1 * time.Hour},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			db := openSQLiteTestDB(t)
			require.NoError(t, db.AutoMigrate(&Note{}))

			sun := &suncalc.SunEventTimes{Sunrise: base.Add(sc.sunrise), Sunset: base.Add(sc.sunset)}

			// One note per minute of the day, plus one on another date that the
			// bounds do not cover and that must therefore never match.
			want := make(map[string][]uint, len(allTimeOfDayPeriods))
			notes := make([]Note, 0, minutesPerDay+1)
			for minute := range minutesPerDay {
				cand := base.Add(time.Duration(minute) * time.Minute)
				id := uint(minute + 1)
				notes = append(notes, Note{ID: id, Date: clauseTestDate, Time: cand.Format(time.TimeOnly),
					ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.9})
				label := suncalc.ClassifyTimeOfDay(cand, sun)
				want[label] = append(want[label], id)
			}
			notes = append(notes, Note{ID: minutesPerDay + 1, Date: "2025-07-16", Time: "12:00:00",
				ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.9})
			require.NoError(t, db.CreateInBatches(notes, 200).Error)

			bounds := []timeOfDayBounds{boundsFor(clauseTestDate, sun)}
			matched := 0
			for _, period := range allTimeOfDayPeriods {
				got := sunFilteredNoteIDs(t, db, []string{period}, bounds)
				assert.ElementsMatchf(t, want[period], got, "%s: rows matched by %q must be exactly those the classifier labels %q",
					sc.name, period, period)
				matched += len(got)
			}
			assert.Equal(t, minutesPerDay, matched, "the four periods must partition the covered date's rows")

			// Multiple periods are OR-ed: the result is the union of each period's rows.
			combos := [][]string{
				{TimeOfDaySunrise, TimeOfDaySunset},
				{TimeOfDayDay, TimeOfDayNight},
				{TimeOfDaySunrise, TimeOfDayNight},
				allTimeOfDayPeriods,
			}
			for _, periods := range combos {
				var union []uint
				for _, p := range periods {
					union = append(union, want[p]...)
				}
				assert.ElementsMatchf(t, union, sunFilteredNoteIDs(t, db, periods, bounds),
					"%s: %v must match the union of its periods", sc.name, periods)
			}
		})
	}
}

// TestApplyTimeOfDaySunFilter_UnknownPeriodIsNoConstraint verifies that a
// period list with no recognized name leaves the query untouched (no join).
func TestApplyTimeOfDaySunFilter_UnknownPeriodIsNoConstraint(t *testing.T) {
	t.Parallel()
	db := openSQLiteTestDB(t)
	require.NoError(t, db.AutoMigrate(&Note{}))
	base := time.Date(2025, 7, 15, 0, 0, 0, 0, time.UTC)
	bounds := []timeOfDayBounds{boundsFor(clauseTestDate, &suncalc.SunEventTimes{
		Sunrise: base.Add(6 * time.Hour), Sunset: base.Add(21 * time.Hour),
	})}

	query := db.Model(&Note{})
	got, err := applyTimeOfDaySunFilter(query, []string{"bogus"}, bounds)
	require.NoError(t, err)
	assert.Same(t, query, got)

	_, ok := timeOfDayPeriodPredicate("bogus")
	assert.False(t, ok)
}

// TestBuildTimeOfDaySunJoin pins the derived table's shape: one UNION ALL arm
// per date and a fixed number of parameters per date, independent of how many
// periods are requested. That is what keeps the bound-parameter count bounded
// (the replaced form bound a parameter set per date AND per period).
func TestBuildTimeOfDaySunJoin(t *testing.T) {
	t.Parallel()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	const days = maxTimeOfDayFilterDays + 1
	bounds := make([]timeOfDayBounds, 0, days)
	for i := range days {
		d := base.AddDate(0, 0, i)
		bounds = append(bounds, boundsFor(d.Format(time.DateOnly), &suncalc.SunEventTimes{
			Sunrise: d.Add(6 * time.Hour), Sunset: d.Add(21 * time.Hour),
		}))
	}

	for _, dialect := range []string{DialectSQLite, DialectMySQL} {
		joinSQL, args, err := buildTimeOfDaySunJoin(bounds, dialect)
		require.NoError(t, err)
		assert.Len(t, args, days*todSunColumnCount, dialect)
		assert.Equal(t, days*todSunColumnCount, strings.Count(joinSQL, "?"), dialect)
		assert.Equal(t, days-1, strings.Count(joinSQL, "UNION ALL"), dialect)
		assert.Equal(t, bounds[0].appendArgs(nil), args[:todSunColumnCount], "the first row's columns must follow the declared order")
	}

	// MySQL cells are converted to ascii so they compare with a notes column in
	// any character set; SQLite has no CONVERT ... USING and binds them bare.
	mysqlSQL, _, err := buildTimeOfDaySunJoin(bounds[:1], DialectMySQL)
	require.NoError(t, err)
	assert.Equal(t, todSunColumnCount, strings.Count(mysqlSQL, todSunParamMySQL))
	sqliteSQL, _, err := buildTimeOfDaySunJoin(bounds[:1], DialectSQLite)
	require.NoError(t, err)
	assert.NotContains(t, sqliteSQL, "CONVERT")

	t.Run("rejects empty bounds", func(t *testing.T) {
		t.Parallel()
		_, _, err := buildTimeOfDaySunJoin(nil, DialectSQLite)
		require.Error(t, err)
	})
	t.Run("rejects more rows than a SQLite compound select allows", func(t *testing.T) {
		t.Parallel()
		tooMany := make([]timeOfDayBounds, maxTimeOfDaySunRows+1)
		_, _, err := buildTimeOfDaySunJoin(tooMany, DialectSQLite)
		require.Error(t, err)
	})
}
