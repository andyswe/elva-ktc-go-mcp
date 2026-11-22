package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var relative = regexp.MustCompile(`^-(\d+)(min|h|d|w|mo|y)$`)

// parseWhen parses a point in time given by a tool caller. Dates and times without
// an offset are local (loc):
//
//	now, -30min, -6h, -7d, -2w, -3mo, -1y     relative to now
//	2026, 2026-11, 2026-11-22                 start of that year, month or day
//	2026-11-22T14:00, 2026-11-22 14:00:30     local time
//	2026-11-22T14:00:00+01:00, ...Z           exact time (RFC 3339)
func parseWhen(s string, loc *time.Location, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "now" {
		return now, nil
	}
	if m := relative.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "min":
			return now.Add(-time.Duration(n) * time.Minute), nil
		case "h":
			return now.Add(-time.Duration(n) * time.Hour), nil
		case "d":
			return now.AddDate(0, 0, -n), nil
		case "w":
			return now.AddDate(0, 0, -7*n), nil
		case "mo":
			return now.AddDate(0, -n, 0), nil
		default:
			return now.AddDate(-n, 0, 0), nil
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02", "2006-01", "2006"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read time %q: use e.g. 2026-11-22, 2026-11, 2026-11-22T14:00, -7d or now", s)
}

// parseRange parses from (required) and to (default now) and checks their order.
func parseRange(from, to string, loc *time.Location, now time.Time) (time.Time, time.Time, error) {
	if from == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("from is required")
	}
	f, err := parseWhen(from, loc, now)
	if err != nil {
		return f, f, err
	}
	t := now
	if to != "" {
		if t, err = parseWhen(to, loc, now); err != nil {
			return f, t, err
		}
	}
	if !f.Before(t) {
		return f, t, fmt.Errorf("from (%s) must be before to (%s)", f.Format(time.RFC3339), t.Format(time.RFC3339))
	}
	return f, t, nil
}
