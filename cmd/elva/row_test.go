package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andyswe/elva-ktc-go-mcp/internal/ktc"
	"github.com/andyswe/elva-ktc-go-mcp/internal/store"
)

func meter(name string, vals ...ktc.Value) *ktc.MeterReading {
	return &ktc.MeterReading{Name: name, Values: vals}
}

func TestToRow(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	m := &Measurement{Time: now, Meters: []*ktc.MeterReading{
		meter("VV-01", ktc.Value{Quantity: "volume", Unit: "m3", Value: 187.588}, ktc.Value{Quantity: "flow", Unit: "l/h", Value: 1.5}),
		meter("EL-01", ktc.Value{Quantity: "energy", Unit: "kWh", Value: 26796.1}, ktc.Value{Quantity: "power", Unit: "W", Value: 512}),
		meter("T-01", ktc.Value{Quantity: "external_temp", Unit: "C", Value: 21}), // ignored
	}}
	r, err := toRow(m)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Time.Equal(now) || *r.ElEnergy != 26796100 || *r.ElPower != 512 || *r.VaVolume != 187588 || *r.VaFlow != 1.5 {
		t.Errorf("row = %+v", r)
	}

	// Only an electricity meter: water columns stay NULL.
	r, err = toRow(&Measurement{Meters: []*ktc.MeterReading{meter("EL-01", ktc.Value{Quantity: "power", Unit: "W", Value: 7})}})
	if err != nil || r.VaVolume != nil || r.VaFlow != nil || r.ElEnergy != nil || *r.ElPower != 7 {
		t.Errorf("partial row = %+v, %v", r, err)
	}

	// Two meters filling the same column must be rejected, not silently mixed.
	_, err = toRow(&Measurement{Meters: []*ktc.MeterReading{
		meter("EL-01", ktc.Value{Quantity: "energy", Unit: "Wh", Value: 1}),
		meter("HEAT-01", ktc.Value{Quantity: "energy", Unit: "kWh", Value: 2}),
	}})
	if err == nil || !strings.Contains(err.Error(), "HEAT-01") {
		t.Errorf("duplicate: err = %v", err)
	}
}

func TestBuffer(t *testing.T) {
	row := func(i int) store.Row { return store.Row{Time: time.Unix(int64(i)*60, 0)} }
	b := &buffer{max: 3}
	for i := range 3 {
		if d := b.add(row(i)); d != 0 {
			t.Fatalf("dropped %d before full", d)
		}
	}
	if d := b.add(row(3)); d != 1 || !b.rows[0].Time.Equal(row(1).Time) {
		t.Fatalf("full buffer: dropped %d, oldest %v", d, b.rows[0].Time)
	}

	fail := errors.New("db down")
	var got []store.Row
	down := true
	insert := func(_ context.Context, rows []store.Row) (int64, error) {
		if down {
			return 0, fail
		}
		got = append(got, rows...)
		return int64(len(rows)), nil
	}
	if n, err := b.flush(context.Background(), insert); n != 0 || err != fail || len(b.rows) != 3 {
		t.Fatalf("flush while down: n=%d err=%v buffered=%d", n, err, len(b.rows))
	}
	down = false
	if n, err := b.flush(context.Background(), insert); n != 3 || err != nil || len(b.rows) != 0 {
		t.Fatalf("flush after recovery: n=%d err=%v buffered=%d", n, err, len(b.rows))
	}
	if len(got) != 3 || !got[0].Time.Equal(row(1).Time) || !got[2].Time.Equal(row(3).Time) {
		t.Errorf("written out of order: %v", got)
	}

	// A large backlog is written in chunks.
	big := &buffer{max: 2500}
	for i := range 2500 {
		big.add(row(i))
	}
	calls := 0
	n, err := big.flush(context.Background(), func(_ context.Context, rows []store.Row) (int64, error) {
		calls++
		return int64(len(rows)), nil
	})
	if n != 2500 || err != nil || calls != 3 {
		t.Errorf("chunked flush: n=%d err=%v calls=%d", n, err, calls)
	}
}
