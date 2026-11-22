package store

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/andyswe/elva-ktc-go-mcp/internal/testdb"
)

var stockholm, _ = time.LoadLocation("Europe/Stockholm")

// fixture writes minute rows from 2026-03-28 00:00 to 2026-03-31 00:00 Stockholm time.
// That spans the spring DST change (2026-03-29 02:00 -> 03:00), so the 29th has 23 h.
//   - power: 100 W at night (01:00-05:00), 500 W otherwise
//   - energy: the integral of power, reported in 100 Wh steps like a real meter
//   - water: +1 l every 20 min from 06:00 to 22:00; on the night of the 29th a 2 l "leak"
//   - a gap: no rows on 2026-03-30 10:00-12:00
func fixture(t *testing.T) (*Reader, time.Time, time.Time) {
	t.Helper()
	url := testdb.URL(t, "test_read")
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "TRUNCATE measurement"); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 3, 28, 0, 0, 0, 0, stockholm)
	to := time.Date(2026, 3, 31, 0, 0, 0, 0, stockholm)
	var rows []Row
	energy, water := 1_000_000.0, int64(5000)
	for ts := from; ts.Before(to); ts = ts.Add(time.Minute) {
		l := ts.In(stockholm)
		power := int32(500)
		if l.Hour() >= 1 && l.Hour() < 5 {
			power = 100
		}
		energy += float64(power) / 60
		if l.Hour() >= 6 && l.Hour() < 22 && l.Minute()%20 == 0 {
			water++
		}
		if l.Day() == 29 && l.Hour() == 3 && (l.Minute() == 10 || l.Minute() == 40) {
			water++ // leak
		}
		if l.Day() == 30 && l.Hour() >= 10 && l.Hour() < 12 {
			continue // gap
		}
		rows = append(rows, Row{
			Time:     ts,
			ElEnergy: ptr(int64(math.Floor(energy/100) * 100)),
			ElPower:  ptr(power),
			VaVolume: ptr(water),
			VaFlow:   ptr(float32(0)),
		})
	}
	if _, err := s.Insert(ctx, rows); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(ctx, url, stockholm, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, from, to
}

func TestReader(t *testing.T) {
	r, from, to := fixture(t)
	ctx := context.Background()

	t.Run("latest", func(t *testing.T) {
		row, err := r.LatestRow(ctx)
		if err != nil || row == nil || !row.Time.Equal(to.Add(-time.Minute)) || *row.ElPower != 500 {
			t.Errorf("LatestRow = %+v, %v", row, err)
		}
	})

	t.Run("consumption per day", func(t *testing.T) {
		days, err := r.Consumption(ctx, from, to, "day")
		if err != nil || len(days) != 3 {
			t.Fatalf("Consumption = %d days, %v", len(days), err)
		}
		// 28th: 4 h x 100 W + 20 h x 500 W = 10.4 kWh. 29th (23 h, the night loses
		// 02:00-03:00): 3 h x 100 W + 20 h x 500 W = 10.3 kWh. 30th: 10.4 kWh; the gap
		// doesn't lose energy because the totals keep counting.
		for i, want := range []float64{10.4, 10.3, 10.4} {
			d := days[i]
			if d.ElKWh == nil || math.Abs(*d.ElKWh-want) > 0.15 {
				t.Errorf("%s: %v kWh, want about %v", d.Start.Format("01-02"), deref(d.ElKWh), want)
			}
			if d.Start.Hour() != 0 || d.Start.Location() != stockholm {
				t.Errorf("bucket start %v is not local midnight", d.Start)
			}
		}
		if !days[0].Partial || days[1].Partial {
			t.Errorf("partial = %v %v, want only the first day partial", days[0].Partial, days[1].Partial)
		}
		if *days[1].WaterL != 2+48 || *days[0].WaterL < 47 { // 48 l during the day plus the leak
			t.Errorf("water = %v %v", *days[0].WaterL, *days[1].WaterL)
		}
		if days[1].Coverage < 0.99 || days[2].Coverage > 0.95 {
			t.Errorf("coverage = %.3f (DST day) %.3f (gap day)", days[1].Coverage, days[2].Coverage)
		}
		if *days[0].MaxPowerW != 500 {
			t.Errorf("max power = %d", *days[0].MaxPowerW)
		}
	})

	t.Run("consumption with a baseline before the range", func(t *testing.T) {
		days, err := r.Consumption(ctx, from.AddDate(0, 0, 1), to, "day")
		if err != nil || len(days) != 2 || days[0].Partial {
			t.Fatalf("Consumption = %+v, %v", days, err)
		}
		if _, err := r.Consumption(ctx, from, to, "fortnight"); err == nil {
			t.Error("bad bucket accepted")
		}
	})

	t.Run("timeseries", func(t *testing.T) {
		pts, err := r.Timeseries(ctx, from, from.AddDate(0, 0, 1), "power", time.Hour)
		if err != nil || len(pts) != 24 {
			t.Fatalf("Timeseries = %d points, %v", len(pts), err)
		}
		if p := pts[2]; p.Min != 100 || p.Max != 100 || p.Rows != 60 || p.Time.Hour() != 2 {
			t.Errorf("02:00 point = %+v", p)
		}
		if _, err := r.Timeseries(ctx, from, to, "el_power; DROP TABLE x", time.Hour); err == nil {
			t.Error("unknown field accepted")
		}
	})

	t.Run("baseload", func(t *testing.T) {
		nights, err := r.Baseload(ctx, from, to, "01:00", "05:00")
		if err != nil || len(nights) != 3 {
			t.Fatalf("Baseload = %+v, %v", nights, err)
		}
		for _, n := range nights {
			if *n.MinW != 100 || *n.P10W != 100 || *n.AvgW != 100 {
				t.Errorf("%s: min %v p10 %v avg %v, want 100", n.Date, *n.MinW, *n.P10W, *n.AvgW)
			}
		}
		if nights[1].Date != "2026-03-29" || *nights[1].WaterL != 2 || *nights[0].WaterL != 0 || nights[1].Rows != 180 {
			t.Errorf("night 29th = %+v (water %v), 28th water %v", nights[1], *nights[1].WaterL, *nights[0].WaterL)
		}
	})

	t.Run("info", func(t *testing.T) {
		d, err := r.Info(ctx, from.AddDate(-1, 0, 0), to.AddDate(1, 0, 0), 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if d.Rows != int64(71*60*60/60-120) || !d.First.Equal(from) || d.Gaps != 1 || d.GapTotal != 2*time.Hour+time.Minute {
			t.Errorf("Info = %+v", d)
		}
		if len(d.LargestGaps) != 1 || d.LargestGaps[0].From.Hour() != 9 {
			t.Errorf("gaps = %+v", d.LargestGaps)
		}
	})

	t.Run("query", func(t *testing.T) {
		res, err := r.Query(ctx, "SELECT time, el_power FROM measurement ORDER BY time LIMIT 5", 3)
		if err != nil || len(res.Rows) != 3 || !res.Truncated || res.Columns[1] != "el_power" {
			t.Fatalf("Query = %+v, %v", res, err)
		}
		if ts, _ := res.Rows[0][0].(string); !strings.HasPrefix(ts, "2026-03-28T00:00:00+01:00") {
			t.Errorf("time not local RFC 3339: %v", res.Rows[0][0])
		}
		// date_trunc runs in the session time zone (Stockholm), not UTC.
		res, err = r.Query(ctx, "SELECT date_trunc('day', min(time))::text FROM measurement", 10)
		if err != nil || res.Rows[0][0] != "2026-03-28 00:00:00+01" {
			t.Errorf("session time zone: %v, %v", res, err)
		}
		// Cells are always scalars: composite values come back as their JSON text.
		res, err = r.Query(ctx, `SELECT '{"a":1}'::jsonb, ARRAY[1,2], 1.5::numeric, NULL::int, true, interval '90 minutes'`, 10)
		if err != nil {
			t.Fatal(err)
		}
		want := []any{`{"a":1}`, "[1,2]", 1.5, nil, true, "01:30:00"}
		for i, w := range want {
			if res.Rows[0][i] != w {
				t.Errorf("cell %d (%s) = %#v, want %#v", i, res.Columns[i], res.Rows[0][i], w)
			}
		}
		for _, bad := range []string{
			"DELETE FROM measurement",
			"SELECT 1; DELETE FROM measurement",
			"CREATE TABLE x (i int)",
		} {
			if _, err := r.Query(ctx, bad, 10); err == nil {
				t.Errorf("%q was allowed", bad)
			}
		}
		if row, _ := r.LatestRow(ctx); row == nil {
			t.Error("data gone after write attempts")
		}
	})
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
