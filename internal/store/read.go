package store

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Reader runs read-only queries for analysis (the MCP server). Every session is
// read-only at the database level, so even the raw Query tool cannot write; use a
// role with SELECT rights only as well.
type Reader struct {
	pool *pgxpool.Pool
	loc  *time.Location
}

// OpenReader connects read-only. Sessions use loc as their time zone (so date_trunc
// and friends work in local time) and stop statements after timeout.
func OpenReader(ctx context.Context, url string, loc *time.Location, timeout time.Duration) (*Reader, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	cfg.MaxConns = 2
	p := cfg.ConnConfig.RuntimeParams
	p["application_name"] = "elva-mcp-stdio"
	p["default_transaction_read_only"] = "on"
	p["statement_timeout"] = fmt.Sprint(timeout.Milliseconds())
	p["TimeZone"] = loc.String()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, "SELECT 1 FROM measurement LIMIT 0"); err != nil {
		pool.Close()
		return nil, fmt.Errorf("checking table measurement: %w", err)
	}
	return &Reader{pool: pool, loc: loc}, nil
}

// Close closes all connections.
func (r *Reader) Close() { r.pool.Close() }

// Location is the time zone used for buckets and output.
func (r *Reader) Location() *time.Location { return r.loc }

// LatestRow returns the newest row, or nil if the table is empty.
func (r *Reader) LatestRow(ctx context.Context) (*Row, error) {
	var row Row
	err := r.pool.QueryRow(ctx, `SELECT time, el_energy, el_power, va_volume, va_flow
		FROM measurement ORDER BY time DESC LIMIT 1`).Scan(&row.Time, &row.ElEnergy, &row.ElPower, &row.VaVolume, &row.VaFlow)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &row, err
}

// Earliest returns the time of the oldest row, or nil if the table is empty.
func (r *Reader) Earliest(ctx context.Context) (*time.Time, error) {
	var t *time.Time
	if err := r.pool.QueryRow(ctx, `SELECT min(time) FROM measurement`).Scan(&t); err != nil {
		return nil, err
	}
	return t, nil
}

// Bucket is consumption in one calendar period.
type Bucket struct {
	Start     time.Time // local start of the period
	End       time.Time
	ElKWh     *float64 // electricity used in the period
	WaterL    *int64   // water used in the period
	AvgPowerW *float64
	MaxPowerW *int32
	Rows      int
	Coverage  float64 // rows / minutes in the (clipped) period; 1 = no gaps
	// Partial is true when consumption is measured from the period's first reading
	// instead of from the previous reading: there was none, or it was more than
	// MaxGap earlier (its consumption is then in a GapUse).
	Partial bool
}

// GapUse is consumption across a gap of more than MaxGap that ends in a bucket. The
// meter totals show how much was used, but not when, so it is not counted in any
// bucket.
type GapUse struct {
	From, To time.Time // last reading before the gap, first reading after it
	ElKWh    *float64
	WaterL   *int64
}

// MaxGap is the longest gap whose consumption Consumption still counts in the
// bucket after it. Longer gaps go to GapUse, so that a period after an outage of
// weeks doesn't show the whole outage's consumption.
const MaxGap = time.Hour

// Buckets supported by Consumption, as PostgreSQL date_trunc fields.
var Buckets = []string{"hour", "day", "week", "month", "year"}

// meter tracks one meter total through Consumption.
type meter struct {
	prev     *int64 // last total seen
	prevTime time.Time
}

// step returns the consumption in a bucket whose readings are lo..hi (first at
// firstT, last at lastT), and the consumption across a gap before it, if longer
// than MaxGap.
func (m *meter) step(lo, hi *int64, firstT, lastT *time.Time) (used, gapUsed *int64, partial bool) {
	if hi == nil {
		return nil, nil, false
	}
	base := m.prev
	switch {
	case base == nil:
		base, partial = lo, true
	case firstT.Sub(m.prevTime) > MaxGap:
		if d := *lo - *base; d >= 0 {
			gapUsed = &d
		}
		base, partial = lo, true
	}
	if d := *hi - *base; d >= 0 {
		used = &d
	} // a negative delta means the meter was replaced or reset: report nothing
	m.prev, m.prevTime = hi, *lastT
	return used, gapUsed, partial
}

// Consumption sums consumption per calendar bucket between from and to. It uses the
// meter totals: consumption in a bucket is its last total minus the last total
// before it, which is correct across short gaps and summer time changes. After a
// gap longer than MaxGap a bucket counts from its own first reading, and the gap's
// consumption is returned separately.
func (r *Reader) Consumption(ctx context.Context, from, to time.Time, bucket string) ([]Bucket, []GapUse, error) {
	if !slices.Contains(Buckets, bucket) {
		return nil, nil, fmt.Errorf("bucket must be one of %v", Buckets)
	}
	var el, va meter
	var elT, vaT *time.Time
	err := r.pool.QueryRow(ctx, `SELECT e.el_energy, e.time, v.va_volume, v.time FROM
		(SELECT 1) one
		LEFT JOIN (SELECT el_energy, time FROM measurement WHERE time < $1 AND el_energy IS NOT NULL ORDER BY time DESC LIMIT 1) e ON true
		LEFT JOIN (SELECT va_volume, time FROM measurement WHERE time < $1 AND va_volume IS NOT NULL ORDER BY time DESC LIMIT 1) v ON true`,
		from).Scan(&el.prev, &elT, &va.prev, &vaT)
	if err != nil {
		return nil, nil, err
	}
	if elT != nil {
		el.prevTime = *elT
	}
	if vaT != nil {
		va.prevTime = *vaT
	}
	rows, err := r.pool.Query(ctx, `SELECT date_trunc($1, time AT TIME ZONE $2),
			min(el_energy), max(el_energy), min(time) FILTER (WHERE el_energy IS NOT NULL), max(time) FILTER (WHERE el_energy IS NOT NULL),
			min(va_volume), max(va_volume), min(time) FILTER (WHERE va_volume IS NOT NULL), max(time) FILTER (WHERE va_volume IS NOT NULL),
			avg(el_power)::float8, max(el_power), count(*)
		FROM measurement WHERE time >= $3 AND time < $4
		GROUP BY 1 ORDER BY 1`, bucket, r.loc.String(), from, to)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var (
		out  []Bucket
		gaps []GapUse
	)
	for rows.Next() {
		var (
			b                          Bucket
			local                      time.Time
			elMin, elMax, vaMin, vaMax *int64
			elT0, elT1, vaT0, vaT1     *time.Time
		)
		if err := rows.Scan(&local, &elMin, &elMax, &elT0, &elT1, &vaMin, &vaMax, &vaT0, &vaT1,
			&b.AvgPowerW, &b.MaxPowerW, &b.Rows); err != nil {
			return nil, nil, err
		}
		b.Start = time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, r.loc)
		b.End = nextBucket(b.Start, bucket)
		elPrevT, vaPrevT := el.prevTime, va.prevTime
		elUsed, elGap, elPartial := el.step(elMin, elMax, elT0, elT1)
		vaUsed, vaGap, vaPartial := va.step(vaMin, vaMax, vaT0, vaT1)
		b.Partial = elPartial || vaPartial
		if elUsed != nil {
			b.ElKWh = ptr(float64(*elUsed) / 1000)
		}
		b.WaterL = vaUsed
		if elGap != nil {
			gaps = addGap(gaps, GapUse{From: elPrevT.In(r.loc), To: elT0.In(r.loc), ElKWh: ptr(float64(*elGap) / 1000)})
		}
		if vaGap != nil {
			gaps = addGap(gaps, GapUse{From: vaPrevT.In(r.loc), To: vaT0.In(r.loc), WaterL: vaGap})
		}
		start, end := maxTime(b.Start, from), minTime(b.End, to)
		if minutes := end.Sub(start).Minutes(); minutes > 0 {
			b.Coverage = math.Min(1, float64(b.Rows)/minutes)
		}
		out = append(out, b)
	}
	return out, gaps, rows.Err()
}

// addGap records an electricity or a water gap, merging it into the previous entry
// when both meters have the same gap (the normal case: whole rows missing).
func addGap(gaps []GapUse, g GapUse) []GapUse {
	if n := len(gaps); n > 0 && gaps[n-1].From.Equal(g.From) && gaps[n-1].To.Equal(g.To) {
		gaps[n-1].WaterL = g.WaterL // el is always added first
		return gaps
	}
	return append(gaps, g)
}

func nextBucket(t time.Time, bucket string) time.Time {
	switch bucket {
	case "hour":
		return t.Add(time.Hour)
	case "day":
		return t.AddDate(0, 0, 1)
	case "week":
		return t.AddDate(0, 0, 7)
	case "month":
		return t.AddDate(0, 1, 0)
	}
	return t.AddDate(1, 0, 0)
}

// Point is one step of a downsampled time series.
type Point struct {
	Time          time.Time
	Min, Avg, Max float64
	Rows          int
}

// Series are the columns Timeseries can return.
var Series = map[string]string{"power": "el_power", "flow": "va_flow"}

// Timeseries returns min/avg/max of power (W) or flow (l/h) per step between from and to.
func (r *Reader) Timeseries(ctx context.Context, from, to time.Time, field string, step time.Duration) ([]Point, error) {
	col, ok := Series[field]
	if !ok {
		return nil, fmt.Errorf("field must be power or flow")
	}
	// col comes from the fixed map above, never from the caller.
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`SELECT date_bin($1::interval, time, $2::timestamptz),
			min(%[1]s)::float8, avg(%[1]s)::float8, max(%[1]s)::float8, count(*)
		FROM measurement WHERE time >= $2 AND time < $3 AND %[1]s IS NOT NULL
		GROUP BY 1 ORDER BY 1`, col), step, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.Time, &p.Min, &p.Avg, &p.Max, &p.Rows); err != nil {
			return nil, err
		}
		p.Time = p.Time.In(r.loc)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Night is the consumption during one night's window.
type Night struct {
	Date   string // local date of the night window, YYYY-MM-DD
	MinW   *float64
	P10W   *float64 // power stays above this 90 % of the window
	AvgW   *float64
	WaterL *int64 // water used within the window
	Rows   int
}

// Baseload returns, per night between from and to, power statistics and water use
// within the local time window [start, end), e.g. 01:00-05:00.
func (r *Reader) Baseload(ctx context.Context, from, to time.Time, start, end string) ([]Night, error) {
	rows, err := r.pool.Query(ctx, `SELECT to_char((time AT TIME ZONE $1)::date, 'YYYY-MM-DD'),
			min(el_power)::float8,
			percentile_cont(0.1) WITHIN GROUP (ORDER BY el_power),
			avg(el_power)::float8,
			max(va_volume) - min(va_volume),
			count(*)
		FROM measurement
		WHERE time >= $2 AND time < $3
		  AND (time AT TIME ZONE $1)::time >= $4::time AND (time AT TIME ZONE $1)::time < $5::time
		GROUP BY 1 ORDER BY 1`, r.loc.String(), from, to, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Night
	for rows.Next() {
		var n Night
		if err := rows.Scan(&n.Date, &n.MinW, &n.P10W, &n.AvgW, &n.WaterL, &n.Rows); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Gap is a period without rows.
type Gap struct {
	From, To time.Time
}

// DataInfo describes the stored data between from and to.
type DataInfo struct {
	Rows          int64
	First, Last   *time.Time
	RowsWithEl    int64
	RowsWithWater int64
	Gaps          int64         // number of gaps longer than the threshold
	GapTotal      time.Duration // their combined length
	LargestGaps   []Gap         // up to 20, longest first
}

// Info summarises rows and gaps longer than minGap between from and to.
func (r *Reader) Info(ctx context.Context, from, to time.Time, minGap time.Duration) (DataInfo, error) {
	var d DataInfo
	err := r.pool.QueryRow(ctx, `SELECT count(*), min(time), max(time), count(el_energy), count(va_volume)
		FROM measurement WHERE time >= $1 AND time < $2`, from, to).
		Scan(&d.Rows, &d.First, &d.Last, &d.RowsWithEl, &d.RowsWithWater)
	if err != nil {
		return d, err
	}
	const gaps = `WITH g AS (
			SELECT lag(time) OVER (ORDER BY time) AS prev, time
			FROM measurement WHERE time >= $1 AND time < $2)
		SELECT prev, time FROM g WHERE time - prev > $3`
	var total *time.Duration
	if err := r.pool.QueryRow(ctx, `SELECT count(*), sum(time - prev) FROM (`+gaps+`) x`, from, to, minGap).Scan(&d.Gaps, &total); err != nil {
		return d, err
	}
	if total != nil {
		d.GapTotal = *total
	}
	rows, err := r.pool.Query(ctx, gaps+` ORDER BY time - prev DESC LIMIT 20`, from, to, minGap)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var g Gap
		if err := rows.Scan(&g.From, &g.To); err != nil {
			return d, err
		}
		g.From, g.To = g.From.In(r.loc), g.To.In(r.loc)
		d.LargestGaps = append(d.LargestGaps, g)
	}
	if d.First != nil {
		d.First, d.Last = ptr(d.First.In(r.loc)), ptr(d.Last.In(r.loc))
	}
	return d, rows.Err()
}

// QueryResult is the result of a raw read-only query.
type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`      // each cell is a string, number, boolean or nil
	Truncated bool     `json:"truncated"` // more rows existed than maxRows
}

// Query runs one SQL statement in a read-only transaction and returns up to maxRows
// rows. Only a single statement is accepted (the extended protocol refuses more).
func (r *Reader) Query(ctx context.Context, sql string, maxRows int) (*QueryResult, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := &QueryResult{Rows: [][]any{}}
	for _, f := range rows.FieldDescriptions() {
		res.Columns = append(res.Columns, f.Name)
	}
	for rows.Next() {
		if len(res.Rows) == maxRows {
			res.Truncated = true
			break
		}
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		for i, v := range vals {
			vals[i] = r.jsonValue(v)
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}

// jsonValue turns a database value into a JSON scalar: string, number, boolean or nil.
// Times become local RFC 3339 strings; numerics become numbers; anything composite
// (json, arrays, ranges, ...) becomes its JSON text as a string, so the query tool's
// output schema stays simple and portable.
func (r *Reader) jsonValue(v any) any {
	switch x := v.(type) {
	case nil, bool, string, int16, int32, int64, float32, float64:
		return x
	case time.Time:
		return x.In(r.loc).Format(time.RFC3339)
	case time.Duration:
		return x.String()
	case []byte:
		return string(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var scalar any
	if json.Unmarshal(b, &scalar) == nil {
		switch scalar.(type) {
		case nil, bool, string, float64:
			return scalar // e.g. numeric -> number, uuid -> string
		}
	}
	// pgx types without a natural JSON form (interval, ranges, ...) know their
	// PostgreSQL text form.
	if dv, ok := v.(driver.Valuer); ok {
		if text, err := dv.Value(); err == nil {
			if str, ok := text.(string); ok {
				return str
			}
		}
	}
	return string(b) // json, arrays: their JSON text
}

func ptr[T any](v T) *T { return &v }

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
