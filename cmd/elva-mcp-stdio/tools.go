package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/andyswe/elva-ktc-go-mcp/internal/store"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `ElVa records an apartment's electricity and water meters once a minute (read from
a KTC RCU M-Bus node) into PostgreSQL. Use these tools to answer questions about
consumption, power use, standby load and water use.

- Times: all output is local time with UTC offset. Inputs accept 2026-11-22, 2026-11,
  2026-11-22T14:00, RFC 3339, or relative values like -7d, -3mo, now.
- Electricity: el_energy is the meter total in Wh and only moves in 100 Wh steps, so use
  power (W) for short-term detail and get_consumption for energy over periods.
- Water: va_volume is the meter total in litres. The water meter may cover hot water only.
- Gaps: periods without rows happen (outages). Consumption computed from totals stays
  correct across gaps; check coverage_pct and get_data_info before trusting averages.
- Prefer get_consumption, get_timeseries and get_baseload; use query for anything else.`

const schemaDoc = `# ElVa database

One table, one row per minute (older imported data may be irregular):

    CREATE TABLE measurement (
        time       timestamptz PRIMARY KEY,  -- scrape time
        el_energy  bigint,                   -- electricity meter total, Wh (100 Wh steps)
        el_power   integer,                  -- electricity momentary power, W
        va_volume  bigint,                   -- water meter total, l
        va_flow    real                      -- water momentary flow, l/h (imprecise)
    );

- Any column except time may be NULL.
- Consumption over a period = total at its end minus total at its start, e.g.
  electricity per day in kWh:
      SELECT date_trunc('day', time) AS day, (max(el_energy) - min(el_energy)) / 1000.0
      FROM measurement GROUP BY 1 ORDER BY 1;
  (min/max within a period misses the minute before it; get_consumption is exact.)
- The query tool runs in a read-only transaction whose session time zone is %s, so
  date_trunc and casts to date use local time.
`

// round to n decimals.
func round(x float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(x*p) / p
}

func roundPtr(x *float64, n int) *float64 {
	if x == nil {
		return nil
	}
	return ptr(round(*x, n))
}

func ptr[T any](v T) *T { return &v }

type server struct {
	db      *store.Reader
	loc     *time.Location
	maxRows int
	now     func() time.Time
}

func (s *server) ts(t time.Time) string { return t.In(s.loc).Format(time.RFC3339) }

// newServer builds the MCP server with all tools and the schema resource.
func newServer(log *slog.Logger, s *server) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "elva", Title: "ElVa electricity and water", Version: version},
		&mcp.ServerOptions{Instructions: instructions})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}

	add(log, srv, &mcp.Tool{Name: "get_latest", Annotations: ro,
		Description: "Latest stored reading: momentary power (W) and water flow, meter totals, and how old the reading is."},
		s.latest)
	add(log, srv, &mcp.Tool{Name: "get_consumption", Annotations: ro,
		Description: "Electricity (kWh) and water (litres) used per hour, day, week, month or year, computed from the " +
			"meter totals (correct across gaps and DST), with average and peak power per period. " +
			"Buckets follow local calendar periods; weeks start on Monday."},
		s.consumption)
	add(log, srv, &mcp.Tool{Name: "get_timeseries", Annotations: ro,
		Description: "Power (W) or water flow (l/h) over time, downsampled to at most max_points steps with min/avg/max per step. " +
			"Use for curves and to find when something happened."},
		s.timeseries)
	add(log, srv, &mcp.Tool{Name: "get_baseload", Annotations: ro,
		Description: "Standby load and night-time water use per night: power statistics and litres used within a night " +
			"window (default 01:00-05:00 local), plus a summary with typical baseload, its yearly energy and trend. " +
			"Water used at night on many nights suggests a leak."},
		s.baseload)
	add(log, srv, &mcp.Tool{Name: "get_data_info", Annotations: ro,
		Description: "What data exists: row counts, first and last reading, and gaps (periods without readings). " +
			"Check this before comparing periods or trusting averages."},
		s.dataInfo)
	add(log, srv, &mcp.Tool{Name: "query", Annotations: ro, OutputSchema: queryOutputSchema,
		Description: fmt.Sprintf("Run one read-only SQL SELECT against the measurement table (see resource elva://schema). "+
			"Session time zone is %s. At most %d rows are returned; aggregate in SQL instead of fetching raw minutes.",
			s.loc, s.maxRows)},
		s.query)

	srv.AddResource(&mcp.Resource{URI: "elva://schema", Name: "schema", Title: "ElVa database schema", MIMEType: "text/markdown",
		Description: "The measurement table, units and how to compute consumption."},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
				URI: "elva://schema", MIMEType: "text/markdown", Text: fmt.Sprintf(schemaDoc, s.loc),
			}}}, nil
		})
	return srv
}

// add registers a tool with portable schemas (see schemaFor) and logs every call
// (to stderr; stdout is the MCP protocol).
func add[In, Out any](log *slog.Logger, srv *mcp.Server, t *mcp.Tool, h func(context.Context, In) (Out, error)) {
	var err error
	if t.InputSchema, err = schemaFor[In](); err != nil {
		panic(fmt.Sprintf("input schema of %s: %v", t.Name, err))
	}
	if t.OutputSchema == nil { // a tool may bring its own, see queryOutputSchema
		if t.OutputSchema, err = schemaFor[Out](); err != nil {
			panic(fmt.Sprintf("output schema of %s: %v", t.Name, err))
		}
	}
	mcp.AddTool(srv, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		out, err := h(ctx, in)
		if err != nil {
			log.Warn("tool failed", "tool", t.Name, "duration", time.Since(start).Round(time.Millisecond), "err", err)
		} else {
			log.Info("tool", "tool", t.Name, "duration", time.Since(start).Round(time.Millisecond))
		}
		return nil, out, err
	})
}

// --- get_latest

type latestIn struct{}

type latestOut struct {
	Time       string   `json:"time"`
	AgeSeconds int      `json:"age_seconds"`
	PowerW     *int32   `json:"power_w"`
	EnergyKWh  *float64 `json:"energy_total_kwh"`
	FlowLPerH  *float32 `json:"water_flow_l_per_h"`
	VolumeL    *int64   `json:"water_total_l"`
}

func (s *server) latest(ctx context.Context, _ latestIn) (latestOut, error) {
	r, err := s.db.LatestRow(ctx)
	if err != nil || r == nil {
		if err == nil {
			err = fmt.Errorf("no data stored yet")
		}
		return latestOut{}, err
	}
	out := latestOut{Time: s.ts(r.Time), AgeSeconds: int(s.now().Sub(r.Time).Seconds()),
		PowerW: r.ElPower, FlowLPerH: r.VaFlow, VolumeL: r.VaVolume}
	if r.ElEnergy != nil {
		out.EnergyKWh = ptr(float64(*r.ElEnergy) / 1000)
	}
	return out, nil
}

// --- get_consumption

type rangeIn struct {
	From string `json:"from" jsonschema:"start, e.g. 2026-11-01, 2026-11, -7d"`
	To   string `json:"to,omitempty" jsonschema:"end (exclusive), default now"`
}

type consumptionIn struct {
	rangeIn
	Bucket string `json:"bucket" jsonschema:"period length: hour, day, week, month or year"`
}

type bucketOut struct {
	Start       string   `json:"start"`
	ElKWh       *float64 `json:"electricity_kwh"`
	WaterL      *int64   `json:"water_l"`
	AvgPowerW   *float64 `json:"avg_power_w"`
	MaxPowerW   *int32   `json:"max_power_w"`
	CoveragePct float64  `json:"coverage_pct"`
	Partial     bool     `json:"partial,omitempty"`
}

type consumptionOut struct {
	From        string      `json:"from"`
	To          string      `json:"to"`
	Bucket      string      `json:"bucket"`
	TotalKWh    float64     `json:"total_electricity_kwh"`
	TotalWaterL int64       `json:"total_water_l"`
	Buckets     []bucketOut `json:"buckets"`
	Notes       []string    `json:"notes,omitempty"`
}

var approxBucket = map[string]time.Duration{"hour": time.Hour, "day": 24 * time.Hour, "week": 7 * 24 * time.Hour, "month": 28 * 24 * time.Hour, "year": 365 * 24 * time.Hour}

const maxBuckets = 1000

func (s *server) consumption(ctx context.Context, in consumptionIn) (consumptionOut, error) {
	from, to, err := parseRange(in.From, in.To, s.loc, s.now())
	if err != nil {
		return consumptionOut{}, err
	}
	if d, ok := approxBucket[in.Bucket]; ok && to.Sub(from)/d > maxBuckets {
		return consumptionOut{}, fmt.Errorf("more than %d %s buckets: use a larger bucket or a shorter range", maxBuckets, in.Bucket)
	}
	bs, err := s.db.Consumption(ctx, from, to, in.Bucket)
	if err != nil {
		return consumptionOut{}, err
	}
	out := consumptionOut{From: s.ts(from), To: s.ts(to), Bucket: in.Bucket, Buckets: []bucketOut{}}
	lowCoverage := false
	for _, b := range bs {
		out.Buckets = append(out.Buckets, bucketOut{Start: s.ts(b.Start), ElKWh: roundPtr(b.ElKWh, 3), WaterL: b.WaterL,
			AvgPowerW: roundPtr(b.AvgPowerW, 0), MaxPowerW: b.MaxPowerW, CoveragePct: round(b.Coverage*100, 1), Partial: b.Partial})
		if b.ElKWh != nil {
			out.TotalKWh += *b.ElKWh
		}
		if b.WaterL != nil {
			out.TotalWaterL += *b.WaterL
		}
		lowCoverage = lowCoverage || b.Coverage < 0.9
	}
	out.TotalKWh = round(out.TotalKWh, 3)
	if len(bs) > 0 && bs[0].Partial {
		out.Notes = append(out.Notes, "the first period is partial: there is no reading before it, so it counts from its first reading")
	}
	if lowCoverage {
		out.Notes = append(out.Notes, "some periods have coverage below 90 %: consumption is still correct, but averages and peaks miss the gaps, and a gap's consumption lands in the period after it")
	}
	if len(bs) == 0 {
		out.Notes = append(out.Notes, "no readings in this range")
	}
	return out, nil
}

// --- get_timeseries

type timeseriesIn struct {
	rangeIn
	Field     string `json:"field" jsonschema:"power (electricity, W) or flow (water, l/h)"`
	MaxPoints int    `json:"max_points,omitempty" jsonschema:"maximum number of steps, default 500, at most 2000"`
}

type pointOut struct {
	Time string  `json:"time"`
	Min  float64 `json:"min"`
	Avg  float64 `json:"avg"`
	Max  float64 `json:"max"`
}

type timeseriesOut struct {
	Field  string     `json:"field"`
	Unit   string     `json:"unit"`
	Step   string     `json:"step"`
	Points []pointOut `json:"points"`
}

func (s *server) timeseries(ctx context.Context, in timeseriesIn) (timeseriesOut, error) {
	from, to, err := parseRange(in.From, in.To, s.loc, s.now())
	if err != nil {
		return timeseriesOut{}, err
	}
	n := in.MaxPoints
	if n <= 0 {
		n = 500
	}
	n = min(n, 2000)
	step := max(time.Minute, (to.Sub(from)/time.Duration(n) + time.Minute - 1).Truncate(time.Minute))
	pts, err := s.db.Timeseries(ctx, from, to, in.Field, step)
	if err != nil {
		return timeseriesOut{}, err
	}
	out := timeseriesOut{Field: in.Field, Unit: map[string]string{"power": "W", "flow": "l/h"}[in.Field], Step: step.String(), Points: []pointOut{}}
	for _, p := range pts {
		out.Points = append(out.Points, pointOut{Time: s.ts(p.Time), Min: round(p.Min, 1), Avg: round(p.Avg, 1), Max: round(p.Max, 1)})
	}
	return out, nil
}

// --- get_baseload

type baseloadIn struct {
	rangeIn
	NightStart string `json:"night_start,omitempty" jsonschema:"start of the night window, local HH:MM, default 01:00"`
	NightEnd   string `json:"night_end,omitempty" jsonschema:"end of the night window, local HH:MM, default 05:00; must be after night_start"`
	Group      string `json:"group,omitempty" jsonschema:"night, week or month; default: night up to 62 nights, week up to a year, month beyond"`
}

// periodOut summarises the nights of one week or month.
type periodOut struct {
	Period          string   `json:"period"` // week: date of its Monday; month: YYYY-MM
	Nights          int      `json:"nights"`
	MedianP10W      *float64 `json:"median_p10_w"`
	MedianMinW      *float64 `json:"median_min_w"`
	NightsWithWater int      `json:"nights_with_water_use"`
	WaterL          int64    `json:"water_l"`
}

type nightOut struct {
	Date   string   `json:"date"`
	MinW   *float64 `json:"min_w"`
	P10W   *float64 `json:"p10_w"`
	AvgW   *float64 `json:"avg_w"`
	WaterL *int64   `json:"water_l"`
}

type baseloadSummary struct {
	Nights            int      `json:"nights"`
	TypicalBaseloadW  *float64 `json:"typical_baseload_w"`
	YearlyKWh         *float64 `json:"baseload_kwh_per_year"`
	MedianMinW        *float64 `json:"median_min_w"`
	NightsWithWater   int      `json:"nights_with_water_use"`
	FirstHalfMedianW  *float64 `json:"first_half_median_p10_w,omitempty"`
	SecondHalfMedianW *float64 `json:"second_half_median_p10_w,omitempty"`
}

type baseloadOut struct {
	Window  string          `json:"window"`
	Group   string          `json:"group"`
	Summary baseloadSummary `json:"summary"`
	Nights  []nightOut      `json:"nights,omitempty"`  // group=night
	Periods []periodOut     `json:"periods,omitempty"` // group=week or month
	Notes   []string        `json:"notes"`
}

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func (s *server) baseload(ctx context.Context, in baseloadIn) (baseloadOut, error) {
	from, to, err := parseRange(in.From, in.To, s.loc, s.now())
	if err != nil {
		return baseloadOut{}, err
	}
	start, end := in.NightStart, in.NightEnd
	if start == "" {
		start = "01:00"
	}
	if end == "" {
		end = "05:00"
	}
	if !hhmm.MatchString(start) || !hhmm.MatchString(end) || start >= end {
		return baseloadOut{}, fmt.Errorf("night window %s-%s: use HH:MM with night_start before night_end (windows across midnight are not supported)", start, end)
	}
	if !slices.Contains([]string{"", "night", "week", "month"}, in.Group) {
		return baseloadOut{}, fmt.Errorf("group must be night, week or month")
	}
	ns, err := s.db.Baseload(ctx, from, to, start, end)
	if err != nil {
		return baseloadOut{}, err
	}
	group := in.Group
	if group == "" {
		// Keep the answer small enough for a model's context.
		switch {
		case len(ns) <= 62:
			group = "night"
		case len(ns) <= 366:
			group = "week"
		default:
			group = "month"
		}
	}
	out := baseloadOut{Window: start + "-" + end + " " + s.loc.String(), Group: group, Notes: []string{
		"p10_w is the level power stays above 90 % of the window: the most robust baseload measure; min_w is the single lowest minute",
		"avg_w includes fridge/freezer cycles and anything run at night (dishwasher timers, car charging)",
		"water_l counts whole litres used inside the window; regular non-zero nights suggest a leak or dripping tap",
	}}
	var p10s, mins []float64
	for _, n := range ns {
		if group == "night" {
			out.Nights = append(out.Nights, nightOut{Date: n.Date, MinW: roundPtr(n.MinW, 0), P10W: roundPtr(n.P10W, 0), AvgW: roundPtr(n.AvgW, 0), WaterL: n.WaterL})
		}
		if n.P10W != nil {
			p10s = append(p10s, *n.P10W)
		}
		if n.MinW != nil {
			mins = append(mins, *n.MinW)
		}
		if n.WaterL != nil && *n.WaterL > 0 {
			out.Summary.NightsWithWater++
		}
	}
	out.Summary.Nights = len(ns)
	if m, ok := median(p10s); ok {
		out.Summary.TypicalBaseloadW = ptr(round(m, 0))
		out.Summary.YearlyKWh = ptr(round(m*8.76, 0)) // W x 8760 h / 1000
	}
	if m, ok := median(mins); ok {
		out.Summary.MedianMinW = ptr(round(m, 0))
	}
	if group != "night" {
		out.Periods = periods(ns, group)
	}
	if len(p10s) >= 14 { // enough nights to talk about a trend
		a, _ := median(p10s[:len(p10s)/2])
		b, _ := median(p10s[len(p10s)/2:])
		out.Summary.FirstHalfMedianW, out.Summary.SecondHalfMedianW = ptr(round(a, 0)), ptr(round(b, 0))
	}
	return out, nil
}

// periods groups nights by ISO week (keyed by its Monday) or by month.
func periods(ns []store.Night, group string) []periodOut {
	type acc struct {
		p10s, mins []float64
		out        periodOut
	}
	var keys []string
	accs := map[string]*acc{}
	for _, n := range ns {
		key := n.Date[:7] // YYYY-MM
		if group == "week" {
			d, _ := time.Parse(time.DateOnly, n.Date)
			key = d.AddDate(0, 0, -(int(d.Weekday())+6)%7).Format(time.DateOnly)
		}
		a := accs[key]
		if a == nil {
			a = &acc{out: periodOut{Period: key}}
			accs[key] = a
			keys = append(keys, key)
		}
		a.out.Nights++
		if n.P10W != nil {
			a.p10s = append(a.p10s, *n.P10W)
		}
		if n.MinW != nil {
			a.mins = append(a.mins, *n.MinW)
		}
		if n.WaterL != nil && *n.WaterL > 0 {
			a.out.NightsWithWater++
			a.out.WaterL += *n.WaterL
		}
	}
	out := make([]periodOut, 0, len(keys))
	for _, k := range keys {
		a := accs[k]
		if m, ok := median(a.p10s); ok {
			a.out.MedianP10W = ptr(round(m, 0))
		}
		if m, ok := median(a.mins); ok {
			a.out.MedianMinW = ptr(round(m, 0))
		}
		out = append(out, a.out)
	}
	return out
}

func median(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2], true
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2, true
}

// --- get_data_info

type dataInfoIn struct {
	From          string `json:"from,omitempty" jsonschema:"start, default: all data"`
	To            string `json:"to,omitempty" jsonschema:"end, default now"`
	MinGapMinutes int    `json:"min_gap_minutes,omitempty" jsonschema:"report gaps longer than this many minutes, default 10"`
}

type gapOut struct {
	From  string  `json:"from"`
	To    string  `json:"to"`
	Hours float64 `json:"hours"`
}

type dataInfoOut struct {
	Rows                int64    `json:"rows"`
	First               *string  `json:"first_reading"`
	Last                *string  `json:"last_reading"`
	RowsWithElectricity int64    `json:"rows_with_electricity"`
	RowsWithWater       int64    `json:"rows_with_water"`
	Gaps                int64    `json:"gaps"`
	GapHoursTotal       float64  `json:"gap_hours_total"`
	LargestGaps         []gapOut `json:"largest_gaps"`
}

func (s *server) dataInfo(ctx context.Context, in dataInfoIn) (dataInfoOut, error) {
	if in.From == "" {
		in.From = "1970"
	}
	from, to, err := parseRange(in.From, in.To, s.loc, s.now())
	if err != nil {
		return dataInfoOut{}, err
	}
	minGap := 10 * time.Minute
	if in.MinGapMinutes > 0 {
		minGap = time.Duration(in.MinGapMinutes) * time.Minute
	}
	d, err := s.db.Info(ctx, from, to.Add(time.Minute), minGap)
	if err != nil {
		return dataInfoOut{}, err
	}
	out := dataInfoOut{Rows: d.Rows, RowsWithElectricity: d.RowsWithEl, RowsWithWater: d.RowsWithWater,
		Gaps: d.Gaps, GapHoursTotal: round(d.GapTotal.Hours(), 1), LargestGaps: []gapOut{}}
	if d.First != nil {
		out.First, out.Last = ptr(s.ts(*d.First)), ptr(s.ts(*d.Last))
	}
	for _, g := range d.LargestGaps {
		out.LargestGaps = append(out.LargestGaps, gapOut{From: s.ts(g.From), To: s.ts(g.To), Hours: round(g.To.Sub(g.From).Hours(), 2)})
	}
	return out, nil
}

// --- query

// queryOutputSchema is written by hand: the inferred one would describe a cell
// (Go any) as the bare schema true, which some MCP clients refuse.
var queryOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"columns": {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "column names"},
		"rows": {Type: "array", Description: "result rows; each row has one cell per column", Items: &jsonschema.Schema{
			Type: "array",
			Items: &jsonschema.Schema{
				Description: "a cell: times are local RFC 3339 strings; JSON, arrays and other composite values are their JSON text",
				AnyOf:       []*jsonschema.Schema{{Type: "string"}, {Type: "number"}, {Type: "boolean"}, {Type: "null"}},
			},
		}},
		"truncated": {Type: "boolean", Description: "true if more rows existed than the row limit"},
	},
	Required: []string{"columns", "rows", "truncated"},
}

type queryIn struct {
	SQL string `json:"sql" jsonschema:"a single read-only SQL statement, e.g. SELECT ... FROM measurement"`
}

func (s *server) query(ctx context.Context, in queryIn) (*store.QueryResult, error) {
	return s.db.Query(ctx, in.SQL, s.maxRows)
}
