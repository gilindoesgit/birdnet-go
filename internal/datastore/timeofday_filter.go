// timeofday_filter.go: SQL filtering of legacy notes by sun-event time of day.
package datastore

import (
	"strings"
	"time"

	"github.com/tphakala/birdnet-go/internal/errors"
	"github.com/tphakala/birdnet-go/internal/logger"
	"github.com/tphakala/birdnet-go/internal/suncalc"
	"gorm.io/gorm"
)

// Names used by the derived "sun bounds" table that the time-of-day filter joins
// against notes. Every column carries a tod_ prefix so none can collide with a
// notes, note_reviews or note_locks column that other filters reference
// unqualified (date, time, id, ...), and so GORM's default "notes.*" projection
// is never ambiguous.
const (
	todSunTable        = "tod_sun"
	todSunDate         = todSunTable + ".tod_date"
	todSunSunrise      = todSunTable + ".tod_sunrise"
	todSunSunset       = todSunTable + ".tod_sunset"
	todSunSunriseStart = todSunTable + ".tod_sunrise_start"
	todSunSunriseEnd   = todSunTable + ".tod_sunrise_end"
	todSunSunsetStart  = todSunTable + ".tod_sunset_start"
	todSunSunsetEnd    = todSunTable + ".tod_sunset_end"

	// todSunColumns lists the derived table's column names in timeOfDayBounds.appendArgs
	// order. Only the first UNION ALL row names them; later rows inherit them
	// positionally.
	todSunColumns = "tod_date,tod_sunrise,tod_sunset,tod_sunrise_start,tod_sunrise_end,tod_sunset_start,tod_sunset_end"

	// todSunParam is one derived-table cell on SQLite.
	todSunParam = "?"
	// todSunParamMySQL is one derived-table cell on MySQL. A derived-table column
	// built from a bound parameter takes the connection character set with a
	// Unicode repertoire, and MySQL refuses to compare it with a notes column in a
	// different character set (for example a latin1 table created under MySQL
	// 5.7's default server charset: "Illegal mix of collations"). Converting the
	// cell to ascii gives it an ASCII repertoire, which MySQL converts to any
	// column charset implicitly, and keeps idx_notes_date usable for the join.
	// The values are "YYYY-MM-DD" / "HH:MM:SS" strings, so nothing is lost.
	todSunParamMySQL = "CONVERT(? USING ascii)"
	// todSunColumnCount is the number of bound parameters per derived-table row.
	todSunColumnCount = 7

	// todNotesTime is the column every time-of-day predicate tests.
	todNotesTime = "notes.time"
)

// maxTimeOfDayFilterDays caps the date range the filter resolves, keeping the
// per-date work bounded.
const maxTimeOfDayFilterDays = 365

// maxTimeOfDaySunRows is the most rows the derived sun table may hold. Each row
// is one arm of a UNION ALL, and SQLite rejects compound selects with more than
// SQLITE_MAX_COMPOUND_SELECT (500 by default) arms. maxTimeOfDayFilterDays keeps
// the row count well below this; the guard only stops a future range increase
// from turning into an opaque SQL error.
const maxTimeOfDaySunRows = 500

// timeOfDayBounds carries the "HH:MM:SS" clock strings the time-of-day filter
// needs for one date's sun events: the date, the sunrise and sunset event times,
// and their +/- SunEventWindow boundaries. Grouping them into a struct avoids a
// long positional parameter list where a same-typed argument could be transposed
// unnoticed.
type timeOfDayBounds struct {
	date         string
	sunrise      string
	sunset       string
	sunriseStart string
	sunriseEnd   string
	sunsetStart  string
	sunsetEnd    string
}

// appendArgs appends the bounds to dst as bound parameters, in todSunColumns
// order, and returns the extended slice.
func (b *timeOfDayBounds) appendArgs(dst []any) []any {
	return append(dst, b.date, b.sunrise, b.sunset, b.sunriseStart, b.sunriseEnd, b.sunsetStart, b.sunsetEnd)
}

// resolveTimeOfDayBounds computes the sun-event bounds of every calendar date in
// [startDateStr, endDateStr], one entry per date for which sun events resolve.
//
// A missing end of the range defaults relative to the other (one year), and a
// fully open range defaults to the last year. Ranges longer than
// maxTimeOfDayFilterDays are rejected. Dates whose sun events cannot be computed
// are skipped, so rows on them match no period.
func resolveTimeOfDayBounds(startDateStr, endDateStr string, sc *suncalc.SunCalc) ([]timeOfDayBounds, error) {
	// Default to a reasonable date range if no dates are provided
	switch {
	case startDateStr == "" && endDateStr == "":
		// Use past year instead of 90 days
		today := time.Now()
		endDateStr = today.Format(time.DateOnly)
		startDateStr = today.AddDate(-1, 0, 0).Format(time.DateOnly) // 1 year ago
		GetLogger().Debug("TimeOfDay filter applied without date range, defaulting to last year",
			logger.String("start_date", startDateStr),
			logger.String("end_date", endDateStr))
	case startDateStr == "":
		// If only end date is provided, use 1 year before that date as start
		endDate, err := time.Parse(time.DateOnly, endDateStr)
		if err == nil {
			startDateStr = endDate.AddDate(-1, 0, 0).Format(time.DateOnly)
		} else {
			startDateStr = endDateStr // Fallback if parsing fails
		}
	case endDateStr == "":
		// If only start date is provided, use that date +1 year or today (whichever is earlier)
		startDate, err := time.Parse(time.DateOnly, startDateStr)
		if err == nil {
			endDate := startDate.AddDate(1, 0, 0)
			today := time.Now()
			if endDate.After(today) {
				endDate = today
			}
			endDateStr = endDate.Format(time.DateOnly)
		} else {
			endDateStr = startDateStr // Fallback if parsing fails
		}
	}

	// Parse dates in local timezone to match how notes store their times.
	// Using time.Parse would return UTC, causing a ~1 second discrepancy in
	// sunrise/sunset calculations vs the actual note timestamps.
	startDate, err := time.ParseInLocation(time.DateOnly, startDateStr, time.Local)
	if err != nil {
		return nil, errors.New(err).
			Component("datastore").
			Category(errors.CategoryValidation).
			Context("operation", "resolve_time_of_day_bounds").
			Context("start_date", startDateStr).
			Build()
	}
	endDate, err := time.ParseInLocation(time.DateOnly, endDateStr, time.Local)
	if err != nil {
		return nil, errors.New(err).
			Component("datastore").
			Category(errors.CategoryValidation).
			Context("operation", "resolve_time_of_day_bounds").
			Context("end_date", endDateStr).
			Build()
	}

	if endDate.Before(startDate) {
		return nil, errors.Newf("end date cannot be before start date").
			Component("datastore").
			Category(errors.CategoryValidation).
			Context("operation", "resolve_time_of_day_bounds").
			Context("start_date", startDateStr).
			Context("end_date", endDateStr).
			Build()
	}

	// Limit date range to avoid excessively long queries
	if endDate.Sub(startDate).Hours() > maxTimeOfDayFilterDays*24 {
		return nil, errors.Newf("date range for TimeOfDay filter cannot exceed %d days", maxTimeOfDayFilterDays).
			Component("datastore").
			Category(errors.CategoryValidation).
			Context("operation", "resolve_time_of_day_bounds").
			Context("start_date", startDateStr).
			Context("end_date", endDateStr).
			Build()
	}

	dayCount := int(endDate.Sub(startDate).Hours()/24) + 1
	bounds := make([]timeOfDayBounds, 0, dayCount)
	window := suncalc.SunEventWindow // sunrise/sunset transition half-width (single source of truth)

	// Resolve each date's OWN sun events, so the SQL filter matches the per-row
	// classifier (suncalc.ClassifyTimeOfDay), which also classifies each detection
	// against its own date's sun events. GetSunEventTimes caches internally, so
	// computing per date adds no meaningful cost while removing the day-to-day
	// drift that let the filter diverge from the per-row label near the solstices.
	for d := startDate; !d.After(endDate); d = d.AddDate(0, 0, 1) {
		dateStr := d.Format(time.DateOnly)
		sunTimes, err := sc.GetSunEventTimes(sunEventAnchor(d))
		if err != nil {
			GetLogger().Warn("Could not get sun times for date, skipping for TimeOfDay filter",
				logger.String("date", dateStr),
				logger.Error(err))
			continue
		}

		// sunTimes carries its wall clock in the station's coordinate-derived timezone
		// (see suncalc.NewSunCalc), which does not necessarily match time.Local (the
		// server process's OS timezone). notes.time is always written in time.Local
		// (see the date parsing above), so every bound must be converted into time.Local
		// before formatting - otherwise the two wall clocks are compared as if they were
		// the same, shifting the filter by whatever offset separates the two zones.
		bounds = append(bounds, timeOfDayBounds{
			date:         dateStr,
			sunrise:      sunTimes.Sunrise.In(time.Local).Format(time.TimeOnly),
			sunset:       sunTimes.Sunset.In(time.Local).Format(time.TimeOnly),
			sunriseStart: sunTimes.Sunrise.Add(-window).In(time.Local).Format(time.TimeOnly),
			sunriseEnd:   sunTimes.Sunrise.Add(window).In(time.Local).Format(time.TimeOnly),
			sunsetStart:  sunTimes.Sunset.Add(-window).In(time.Local).Format(time.TimeOnly),
			sunsetEnd:    sunTimes.Sunset.Add(window).In(time.Local).Format(time.TimeOnly),
		})
	}

	GetLogger().Debug("Resolved sun-event bounds for TimeOfDay filter",
		logger.Int("bounds_count", len(bounds)),
		logger.Int("day_range", dayCount))

	return bounds, nil
}

// applyTimeOfDaySunFilter restricts query to notes whose time falls in any of
// periods, judged against the sun events of the note's own date.
//
// The per-date bounds are inlined as a derived table (a UNION ALL of constant
// rows, portable to SQLite and MySQL 5.7+) and joined on notes.date, so each note
// is tested against one row of bounds by a single predicate per period. The
// earlier form OR-ed one "notes.date = ? AND ..." clause per date and period,
// which every row had to walk and which bound a parameter set per date and
// period. Dates are unique in bounds, so the inner join never duplicates a note,
// and notes on dates absent from bounds are excluded, matching a per-date clause
// that was never emitted.
//
// periods must already be canonical (see NormalizeTimeOfDayPeriods); unknown
// names are ignored, and if none is known the query is returned unchanged.
// bounds must be non-empty.
func applyTimeOfDaySunFilter(query *gorm.DB, periods []string, bounds []timeOfDayBounds) (*gorm.DB, error) {
	predicates := make([]string, 0, len(periods))
	for _, period := range periods {
		if p, ok := timeOfDayPeriodPredicate(period); ok {
			predicates = append(predicates, p)
		}
	}
	if len(predicates) == 0 {
		return query, nil
	}

	var dialect string
	if query.Dialector != nil {
		dialect = query.Name()
	}
	joinSQL, joinArgs, err := buildTimeOfDaySunJoin(bounds, dialect)
	if err != nil {
		return nil, err
	}
	return query.Joins(joinSQL, joinArgs...).Where("(" + strings.Join(predicates, " OR ") + ")"), nil
}

// buildTimeOfDaySunJoin builds the JOIN clause and its parameters that attach a
// date's sun-event bounds (as the todSunTable derived table) to each note.
// dialect is the GORM dialector name; it only selects how each cell is bound.
func buildTimeOfDaySunJoin(bounds []timeOfDayBounds, dialect string) (joinSQL string, args []any, err error) {
	if len(bounds) == 0 || len(bounds) > maxTimeOfDaySunRows {
		return "", nil, errors.Newf("time-of-day sun bounds must hold between 1 and %d dates, got %d",
			maxTimeOfDaySunRows, len(bounds)).
			Component("datastore").
			Category(errors.CategoryValidation).
			Context("operation", "build_time_of_day_sun_join").
			Build()
	}

	cell := todSunParam
	if strings.EqualFold(dialect, DialectMySQL) {
		cell = todSunParamMySQL
	}
	columns := strings.Split(todSunColumns, ",")
	firstCells := make([]string, len(columns))
	nextCells := make([]string, len(columns))
	for i, col := range columns {
		firstCells[i] = cell + " AS " + col
		nextCells[i] = cell
	}
	firstRow := "SELECT " + strings.Join(firstCells, ", ")
	nextRow := " UNION ALL SELECT " + strings.Join(nextCells, ", ")

	var sb strings.Builder
	sb.Grow(len(firstRow) + (len(bounds)-1)*len(nextRow) + len(todSunTable) + len(todSunDate) + len("JOIN () AS  ON  = notes.date"))
	sb.WriteString("JOIN (")
	sb.WriteString(firstRow)
	for range len(bounds) - 1 {
		sb.WriteString(nextRow)
	}
	sb.WriteString(") AS ")
	sb.WriteString(todSunTable)
	sb.WriteString(" ON ")
	sb.WriteString(todSunDate)
	sb.WriteString(" = notes.date")

	args = make([]any, 0, len(bounds)*todSunColumnCount)
	for i := range bounds {
		args = bounds[i].appendArgs(args)
	}
	return sb.String(), args, nil
}

// timeOfDayPeriodPredicate returns the WHERE predicate selecting notes in the
// given time-of-day period, written against the joined todSunTable columns. It
// returns ok=false for an unknown period.
//
// The predicates mirror the per-row classifier suncalc.ClassifyTimeOfDay, which
// checks the sunrise window first, then the sunset window, then the daytime arc,
// and calls everything else night. Every window and arc test handles bounds that
// cross midnight (a high-latitude summer sunset whose local wall clock falls
// after 00:00), deciding the wraparound form per row from the bounds themselves.
func timeOfDayPeriodPredicate(period string) (predicate string, ok bool) {
	sunriseWin := sunWindowPredicate(todSunSunriseStart, todSunSunriseEnd)
	sunsetWin := sunWindowPredicate(todSunSunsetStart, todSunSunsetEnd)
	dayArc := sunArcPredicate(todSunSunrise, todSunSunset)
	switch period {
	case TimeOfDaySunrise:
		return sunriseWin, true
	case TimeOfDaySunset:
		// Exclude the sunrise window: the classifier gives sunrise priority, so a
		// timestamp inside both windows (possible when the two overlap at extreme
		// high latitude) is sunrise, not sunset.
		return "(" + sunsetWin + " AND NOT " + sunriseWin + ")", true
	case TimeOfDayDay:
		// On the daytime arc [sunrise, sunset) but outside both transition windows.
		return "(" + dayArc + " AND NOT " + sunriseWin + " AND NOT " + sunsetWin + ")", true
	case TimeOfDayNight:
		// The complement: outside both transition windows and off the daytime arc.
		return "(NOT " + sunriseWin + " AND NOT " + sunsetWin + " AND NOT " + dayArc + ")", true
	default:
		return "", false
	}
}

// sunWindowPredicate tests notes.time against the inclusive clock window
// [start, end], where start and end are column references. A window whose start
// is later than its end has crossed midnight and matches either side of it.
// "HH:MM:SS" strings order lexicographically, matching the stored notes.time.
func sunWindowPredicate(start, end string) string {
	return "((" + start + " <= " + end + " AND " + todNotesTime + " >= " + start + " AND " + todNotesTime + " <= " + end + ")" +
		" OR (" + start + " > " + end + " AND (" + todNotesTime + " >= " + start + " OR " + todNotesTime + " <= " + end + ")))"
}

// sunArcPredicate tests notes.time against the forward clock arc from start
// (inclusive) to end (exclusive), where start and end are column references. The
// arc wraps midnight when start is later than end (a daytime span whose local
// sunset falls after midnight). Mirrors suncalc's inForwardArc so the SQL filter
// and the per-row classifier agree.
func sunArcPredicate(start, end string) string {
	return "((" + start + " <= " + end + " AND " + todNotesTime + " >= " + start + " AND " + todNotesTime + " < " + end + ")" +
		" OR (" + start + " > " + end + " AND (" + todNotesTime + " >= " + start + " OR " + todNotesTime + " < " + end + ")))"
}
