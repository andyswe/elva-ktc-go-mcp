package main

import (
	"fmt"
	"math"
	"strings"

	"github.com/andyswe/elva-ktc-go-mcp/internal/store"
)

// column maps a meter quantity to its database column and the factors that convert
// each unit the RCU may report into the column's unit.
type column struct {
	name    string
	factors map[string]float64
}

var columns = map[string]column{
	"energy": {"el_energy", map[string]float64{"Wh": 1, "kWh": 1e3, "MWh": 1e6}},
	"power":  {"el_power", map[string]float64{"W": 1, "kW": 1e3}},
	"volume": {"va_volume", map[string]float64{"l": 1, "m3": 1e3}},
	"flow":   {"va_flow", map[string]float64{"l/h": 1, "m3/h": 1e3}},
}

// toRow maps a measurement to a database row. Each column must come from at most one
// meter: two meters measuring the same quantity (e.g. a heat meter's energy next to
// the electricity meter) are an error, resolved by limiting KTC_METERS.
// Quantities without a column (temperatures etc.) are ignored.
func toRow(m *Measurement) (store.Row, error) {
	row := store.Row{Time: m.Time}
	from := map[string]string{} // column -> meter that filled it
	for _, r := range m.Meters {
		for _, v := range r.Values {
			col, ok := columns[v.Quantity]
			if !ok {
				continue
			}
			if other, dup := from[col.name]; dup {
				return row, fmt.Errorf("meters %q and %q both report %s; limit KTC_METERS to one of them", other, r.Name, v.Quantity)
			}
			f, ok := col.factors[v.Unit]
			if !ok {
				return row, fmt.Errorf("meter %q reports %s in unsupported unit %q", r.Name, v.Quantity, v.Unit)
			}
			from[col.name] = r.Name
			x := v.Value * f
			switch col.name {
			case "el_energy":
				row.ElEnergy = ptr(int64(math.Round(x)))
			case "el_power":
				row.ElPower = ptr(int32(math.Round(x)))
			case "va_volume":
				row.VaVolume = ptr(int64(math.Round(x)))
			case "va_flow":
				row.VaFlow = ptr(float32(x))
			}
		}
	}
	return row, nil
}

// storedColumns describes which meter fills which column, for the startup log.
func storedColumns(m *Measurement) string {
	var parts []string
	for _, r := range m.Meters {
		for _, v := range r.Values {
			if col, ok := columns[v.Quantity]; ok {
				parts = append(parts, fmt.Sprintf("%s=%s.%s", col.name, r.Name, v.Quantity))
			}
		}
	}
	return strings.Join(parts, " ")
}

func ptr[T any](v T) *T { return &v }
