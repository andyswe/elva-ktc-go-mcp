package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/andyswe/elva-ktc-go-mcp/internal/store"
	"github.com/andyswe/elva-ktc-go-mcp/internal/testdb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var stockholm, _ = time.LoadLocation("Europe/Stockholm")

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 30, 0, 0, stockholm)
	for in, want := range map[string]time.Time{
		"now":                       now,
		"-7d":                       now.AddDate(0, 0, -7),
		"-3mo":                      now.AddDate(0, -3, 0),
		"-90min":                    now.Add(-90 * time.Minute),
		"2026":                      time.Date(2026, 1, 1, 0, 0, 0, 0, stockholm),
		"2026-11":                   time.Date(2026, 11, 1, 0, 0, 0, 0, stockholm),
		"2026-07-01":                time.Date(2026, 7, 1, 0, 0, 0, 0, stockholm), // summer: +02:00
		"2026-11-22T08:15":          time.Date(2026, 11, 22, 8, 15, 0, 0, stockholm),
		"2026-11-22 08:15:30":       time.Date(2026, 11, 22, 8, 15, 30, 0, stockholm),
		"2026-11-22T07:15:00Z":      time.Date(2026, 11, 22, 7, 15, 0, 0, time.UTC),
		"2026-11-22T08:15:00+01:00": time.Date(2026, 11, 22, 8, 15, 0, 0, stockholm),
	} {
		got, err := parseWhen(in, stockholm, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseWhen(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "-7", "22/11/2026", ""} {
		if _, err := parseWhen(bad, stockholm, now); err == nil {
			t.Errorf("parseWhen(%q) accepted", bad)
		}
	}
	if _, _, err := parseRange("2026-11-22", "2026-11-01", stockholm, now); err == nil {
		t.Error("reversed range accepted")
	}
}

// session starts the MCP server on an in-memory transport against a test database
// holding two days of minute data (2026-11-20 and -21): 120 W at night, 400 W by day.
func session(t *testing.T) *mcp.ClientSession {
	t.Helper()
	url := testdb.URL(t, "test_mcp")
	ctx := context.Background()
	w, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	from := time.Date(2026, 11, 20, 0, 0, 0, 0, stockholm)
	var rows []store.Row
	energy, water := 2_000_000.0, int64(1000)
	for ts := from; ts.Before(from.AddDate(0, 0, 2)); ts = ts.Add(time.Minute) {
		p := int32(400)
		if h := ts.Hour(); h >= 1 && h < 5 {
			p = 120
		}
		energy += float64(p) / 60
		if ts.Hour() == 7 && ts.Minute() < 10 {
			water += 5 // a shower
		}
		rows = append(rows, store.Row{Time: ts, ElEnergy: ptr(int64(energy) / 100 * 100), ElPower: ptr(p), VaVolume: ptr(water), VaFlow: ptr(float32(0))})
	}
	if _, err := w.Insert(ctx, rows); err != nil {
		t.Fatal(err)
	}

	db, err := store.OpenReader(ctx, url, stockholm, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	s := &server{db: db, loc: stockholm, maxRows: 100, now: func() time.Time { return from.AddDate(0, 0, 2) }}
	srv := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)), s)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// unportable lists schema constructs some MCP clients refuse: "type" arrays, and
// bare true/false where a schema object is expected.
func unportable(s map[string]any, path string) []string {
	var out []string
	if _, ok := s["type"].([]any); ok {
		out = append(out, path+": type is an array")
	}
	sub := func(p string, v any) {
		switch x := v.(type) {
		case bool:
			out = append(out, p+": bare boolean schema")
		case map[string]any:
			out = append(out, unportable(x, p)...)
		}
	}
	for _, k := range []string{"items", "additionalProperties", "not", "contains", "propertyNames"} {
		if v, ok := s[k]; ok && !(k == "additionalProperties" && v == false) { // false = no extra properties: fine
			sub(path+"."+k, v)
		}
	}
	for _, k := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		list, _ := s[k].([]any)
		for i, v := range list {
			sub(fmt.Sprintf("%s.%s[%d]", path, k, i), v)
		}
	}
	props, _ := s["properties"].(map[string]any)
	for k, v := range props {
		sub(path+".properties."+k, v)
	}
	return out
}

// call runs a tool and decodes its structured result into out.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decoding %s: %v", name, b, err)
		}
	}
	return res
}

func errText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

func TestTools(t *testing.T) {
	cs := session(t)
	ctx := context.Background()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s not marked read-only", tool.Name)
		}
	}
	if got := strings.Join(names, ","); got != "get_baseload,get_consumption,get_data_info,get_latest,get_timeseries,query" {
		t.Errorf("tools = %s", got)
	}
	schema, _ := json.Marshal(tools.Tools[1].InputSchema) // get_consumption
	if !strings.Contains(string(schema), `"from"`) || !strings.Contains(string(schema), `"bucket"`) {
		t.Errorf("get_consumption schema lacks from/bucket: %s", schema)
	}
	// Portable schemas (see schemaFor): every tool's input and output is an object,
	// and no schema anywhere has a "type" array or is a bare true/false.
	for _, tool := range tools.Tools {
		for kind, sc := range map[string]any{"input": tool.InputSchema, "output": tool.OutputSchema} {
			var doc map[string]any
			b, _ := json.Marshal(sc)
			if json.Unmarshal(b, &doc) != nil || doc["type"] != "object" {
				t.Errorf("%s %s schema is not an object: %.100s", tool.Name, kind, b)
				continue
			}
			for _, problem := range unportable(doc, tool.Name+"."+kind) {
				t.Error(problem)
			}
		}
	}
	latestSchema, _ := json.Marshal(tools.Tools[3].OutputSchema) // get_latest
	if !strings.Contains(string(latestSchema), `"power_w":{"anyOf":[{"type":"null"},{`) {
		t.Errorf("get_latest power_w not rewritten to anyOf: %s", latestSchema)
	}

	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "elva://schema"})
	if err != nil || !strings.Contains(res.Contents[0].Text, "el_energy") {
		t.Errorf("schema resource: %v", err)
	}

	var latest latestOut
	call(t, cs, "get_latest", nil, &latest)
	if *latest.PowerW != 400 || latest.Time != "2026-11-21T23:59:00+01:00" || latest.AgeSeconds != 60 {
		t.Errorf("get_latest = %+v", latest)
	}

	var cons consumptionOut
	call(t, cs, "get_consumption", map[string]any{"from": "2026-11-20", "to": "2026-11-22", "bucket": "day"}, &cons)
	// 4 h x 120 W + 20 h x 400 W = 8.48 kWh per day; 50 l of water per day
	if len(cons.Buckets) != 2 || *cons.Buckets[1].ElKWh < 8.4 || *cons.Buckets[1].ElKWh > 8.6 || *cons.Buckets[1].WaterL != 50 {
		t.Errorf("get_consumption = %+v", cons)
	}
	if cons.Buckets[0].Start != "2026-11-20T00:00:00+01:00" || !cons.Buckets[0].Partial || cons.Buckets[1].CoveragePct != 100 {
		t.Errorf("buckets = %+v", cons.Buckets)
	}

	var ts timeseriesOut
	call(t, cs, "get_timeseries", map[string]any{"from": "2026-11-21", "to": "2026-11-22", "field": "power", "max_points": 24}, &ts)
	if len(ts.Points) != 24 || ts.Step != "1h0m0s" || ts.Unit != "W" || ts.Points[3].Max != 120 {
		t.Errorf("get_timeseries = step %s, %d points", ts.Step, len(ts.Points))
	}

	var bl baseloadOut
	call(t, cs, "get_baseload", map[string]any{"from": "-7d"}, &bl)
	if bl.Summary.Nights != 2 || *bl.Summary.TypicalBaseloadW != 120 || *bl.Summary.YearlyKWh != 1051 || bl.Summary.NightsWithWater != 0 {
		t.Errorf("get_baseload summary = %+v", bl.Summary)
	}
	if bl.Group != "night" || len(bl.Nights) != 2 || bl.Periods != nil {
		t.Errorf("get_baseload default grouping = %s, %d nights", bl.Group, len(bl.Nights))
	}
	var blw baseloadOut
	call(t, cs, "get_baseload", map[string]any{"from": "-7d", "group": "week"}, &blw)
	// 2026-11-20 is a Friday: both nights fall in the week starting Monday the 16th.
	if len(blw.Periods) != 1 || blw.Periods[0].Period != "2026-11-16" || blw.Periods[0].Nights != 2 || *blw.Periods[0].MedianP10W != 120 || blw.Nights != nil {
		t.Errorf("get_baseload per week = %+v", blw.Periods)
	}

	var info dataInfoOut
	call(t, cs, "get_data_info", nil, &info)
	if info.Rows != 2*1440 || info.Gaps != 0 || *info.First != "2026-11-20T00:00:00+01:00" {
		t.Errorf("get_data_info = %+v", info)
	}

	var q store.QueryResult
	call(t, cs, "query", map[string]any{"sql": "SELECT date_trunc('day', time)::date::text AS day, count(*) FROM measurement GROUP BY 1 ORDER BY 1"}, &q)
	if len(q.Rows) != 2 || q.Rows[0][0] != "2026-11-20" || q.Rows[0][1] != float64(1440) {
		t.Errorf("query = %+v", q)
	}

	// Errors come back as tool errors the model can read, not protocol failures.
	for _, bad := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"query", map[string]any{"sql": "DELETE FROM measurement"}, "read-only"},
		{"get_consumption", map[string]any{"from": "2020", "bucket": "hour"}, "more than 1000"},
		{"get_consumption", map[string]any{"from": "yesterday", "bucket": "day"}, "cannot read time"},
		{"get_baseload", map[string]any{"from": "-7d", "night_start": "23:00", "night_end": "05:00"}, "night window"},
		{"get_timeseries", map[string]any{"from": "-1d", "field": "voltage"}, "power or flow"},
	} {
		res := call(t, cs, bad.tool, bad.args, nil)
		if !res.IsError || !strings.Contains(errText(res), bad.want) {
			t.Errorf("%s %v: error %v %q, want %q", bad.tool, bad.args, res.IsError, errText(res), bad.want)
		}
	}
}
