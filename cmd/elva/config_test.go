package main

import (
	"strings"
	"testing"
	"time"

	"github.com/andyswe/elva-ktc-go-mcp/internal/config"
)

func TestParseConfig(t *testing.T) {
	vs := func(over map[string]string) config.Values {
		out := make(config.Values, len(settings))
		for i, s := range settings {
			out[i] = config.Value{Setting: s, Value: s.Default}
			if v, ok := over[s.Name]; ok {
				out[i].Value = v
			}
		}
		return out
	}
	c, err := parseConfig(vs(map[string]string{"KTC_HOST": "h", "KTC_PASSWORD": "p", "KTC_METERS": " EL-01, ,VV-01 "}))
	if err != nil {
		t.Fatal(err)
	}
	if c.interval != time.Minute || c.timeout != 10*time.Second || c.dbBuffer != 168*time.Hour ||
		c.loc.String() != "Europe/Stockholm" || strings.Join(c.meters, "|") != "EL-01|VV-01" {
		t.Errorf("cfg = %+v", c)
	}
	if _, err := parseConfig(vs(nil)); err == nil || !strings.Contains(err.Error(), "KTC_HOST, KTC_PASSWORD") {
		t.Errorf("missing: err = %v", err)
	}
	if _, err := parseConfig(vs(map[string]string{"KTC_HOST": "h", "KTC_PASSWORD": "p", "ELVA_INTERVAL": "soon"})); err == nil {
		t.Error("bad interval accepted")
	}
	if _, err := parseConfig(vs(map[string]string{"KTC_HOST": "h", "KTC_PASSWORD": "p", "KTC_TIMEZONE": "Mars/Olympus"})); err == nil {
		t.Error("bad time zone accepted")
	}
}

func TestMeterAgeProblem(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		meterTime time.Time
		problem   bool
	}{
		{now.Add(-45 * time.Second), false}, // normal: RCU reads every minute
		{now.Add(-119 * time.Second), false},
		{now.Add(5 * time.Second), false}, // small clock skew
		{now.Add(-3 * time.Minute), true}, // missed RCU reads
		{now.Add(-time.Hour), true},       // wrong KTC_TIMEZONE
		{now.Add(time.Hour), true},
		{time.Time{}, true},
	} {
		if got := meterAgeProblem(now, tc.meterTime, 2*time.Minute); (got != "") != tc.problem {
			t.Errorf("meter time %v: problem = %q, want problem %v", tc.meterTime, got, tc.problem)
		}
	}
}
