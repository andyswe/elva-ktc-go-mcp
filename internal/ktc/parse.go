package ktc

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// StartPage is what the RCU shows after login.
type StartPage struct {
	Version string
	Model   string
	Name    string
	Meters  []MeterRef
}

// MeterRef is a row in the start page's meter table.
type MeterRef struct {
	Name  string // e.g. "EL-01"
	Value string // display value incl. unit, e.g. "26795900 Wh"
	Pos   int    // M-Bus object number, MB3 -> 3
}

var (
	reProductRow = regexp.MustCompile(`<tr><th>([^<:]+):</th><td>([^<]*)</td></tr>`)
	reMeterRow   = regexp.MustCompile(`<tr><td>([^<]*)</td><td>([^<]*)</td><td>MB(\d+)</td></tr>`)
)

func parseStartPage(body []byte) (*StartPage, error) {
	p := &StartPage{}
	for _, m := range reProductRow.FindAllSubmatch(body, -1) {
		v := strings.TrimSpace(string(m[2]))
		switch string(m[1]) {
		case "Version":
			p.Version = v
		case "Modell":
			p.Model = v
		case "Namn":
			p.Name = v
		}
	}
	for _, m := range reMeterRow.FindAllSubmatch(body, -1) {
		pos, _ := strconv.Atoi(string(m[3]))
		p.Meters = append(p.Meters, MeterRef{
			Name:  strings.TrimSpace(string(m[1])),
			Value: strings.TrimSpace(string(m[2])),
			Pos:   pos,
		})
	}
	if p.Model == "" && len(p.Meters) == 0 {
		return nil, fmt.Errorf("unrecognised start page after login (not an RCU?)")
	}
	return p, nil
}

// Value is one of the six configurable values of an M-Bus object.
type Value struct {
	Index    int     `json:"index"`    // 1..6 (DBIMB1_ValueN)
	Quantity string  `json:"quantity"` // e.g. "energy", "power", "volume", "flow"
	Unit     string  `json:"unit"`     // e.g. "Wh", "W", "l", "l/h"
	Value    float64 `json:"value"`
}

// MeterReading is the parsed content of /mb.xml for one M-Bus object.
type MeterReading struct {
	Pos        int       `json:"pos"`
	Name       string    `json:"name"`
	MeterID    string    `json:"meter_id"`
	MeterTime  time.Time `json:"meter_time"` // when the RCU last read the meter; zero if unknown
	CommStatus string    `json:"comm_status"`
	Values     []Value   `json:"values"`
	// Consumption per period, in the unit of Value1. Nil when the RCU reports "---".
	CurrentHour  *float64 `json:"current_hour,omitempty"`
	CurrentDay   *float64 `json:"current_day,omitempty"`
	CurrentMonth *float64 `json:"current_month,omitempty"`
	PrevHour     *float64 `json:"prev_hour,omitempty"`
	PrevDay      *float64 `json:"prev_day,omitempty"`
	PrevMonth    *float64 `json:"prev_month,omitempty"`
}

// Get returns the first value with the given quantity.
func (m *MeterReading) Get(quantity string) (Value, bool) {
	for _, v := range m.Values {
		if v.Quantity == quantity {
			return v, true
		}
	}
	return Value{}, false
}

// vifUnits maps DBIpMB1_VIFListN codes (from mb.htm) to quantity and unit.
var vifUnits = map[int][2]string{
	1537: {"energy", "Wh"},
	1538: {"energy", "kWh"},
	1539: {"energy", "MWh"},
	1540: {"volume", "l"},
	1541: {"volume", "m3"},
	1542: {"mass", "kg"},
	1543: {"power", "W"},
	1544: {"power", "kW"},
	1545: {"flow", "l/h"},
	1546: {"flow", "m3/h"},
	1547: {"flow_temp", "C"},
	1548: {"return_temp", "C"},
	1549: {"temp_diff", "K"},
	1550: {"external_temp", "C"},
	1551: {"pressure", "bar"},
}

const vifNone = 1536

// parseMeterXML parses /mb.xml. loc is the RCU's time zone: its timestamps carry none.
func parseMeterXML(body []byte, loc *time.Location) (*MeterReading, error) {
	var doc struct {
		XMLName xml.Name `xml:"data"`
		Fields  []struct {
			XMLName xml.Name
			Value   string `xml:",chardata"`
		} `xml:",any"`
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	// Body is already converted to UTF-8; ignore the ISO-8859-1 declaration.
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing mb.xml: %w", err)
	}
	f := make(map[string]string, len(doc.Fields))
	for _, x := range doc.Fields {
		f[x.XMLName.Local] = strings.TrimSpace(x.Value)
	}

	pos, err := strconv.Atoi(strings.TrimPrefix(f["mbPos"], "MB"))
	if err != nil {
		return nil, fmt.Errorf("parsing mb.xml: bad mbPos %q", f["mbPos"])
	}
	r := &MeterReading{
		Pos:        pos,
		Name:       f["DBIpMB1_Txt"],
		MeterID:    f["DBIpMB1_Id"],
		CommStatus: f["DBIMB1_CommInfo"],
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", f["DBIMB1_Time"], loc); err == nil {
		r.MeterTime = t
	}
	for i := 1; i <= 6; i++ {
		v, ok := num(f[fmt.Sprintf("DBIMB1_Value%d", i)])
		if !ok {
			continue
		}
		vif, _ := strconv.Atoi(f[fmt.Sprintf("DBIpMB1_VIFList%d", i)])
		if vif == vifNone {
			continue
		}
		qu, known := vifUnits[vif]
		if !known {
			qu = [2]string{fmt.Sprintf("vif_%d", vif), ""}
		}
		r.Values = append(r.Values, Value{Index: i, Quantity: qu[0], Unit: qu[1], Value: *v})
	}
	r.CurrentHour, _ = num(f["DBIMB1_CurrentHour"])
	r.CurrentDay, _ = num(f["DBIMB1_CurrentDay"])
	r.CurrentMonth, _ = num(f["DBIMB1_CurrentMonth"])
	r.PrevHour, _ = num(f["DBIMB1_PrevHour"])
	r.PrevDay, _ = num(f["DBIMB1_PrevDay"])
	r.PrevMonth, _ = num(f["DBIMB1_PrevMonth"])
	return r, nil
}

func num(s string) (*float64, bool) {
	if s == "" || s == "---" {
		return nil, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, false
	}
	return &v, true
}
