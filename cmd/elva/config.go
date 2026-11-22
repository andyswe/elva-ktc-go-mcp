package main

import (
	"strings"
	"time"

	"github.com/andyswe/elva-ktc-go-mcp/internal/config"
)

// settings is the single place elva's settings are defined: the table drives
// loading, the startup log and -help.
var settings = []config.Setting{
	{Name: "KTC_HOST", Required: true, Help: "host or host:port of the KTC RCU, e.g. 192.168.1.100"},
	{Name: "KTC_PASSWORD", Required: true, Secret: true, Help: "RCU web UI login password"},
	{Name: "KTC_METERS", Help: "comma-separated meter names to read, e.g. EL-01,VV-01 (default: all meters on the RCU)"},
	{Name: "KTC_TIMEZONE", Default: "Europe/Stockholm", Help: "time zone of the RCU clock (its timestamps carry none)"},
	{Name: "ELVA_INTERVAL", Default: "1m", Help: "scrape interval, aligned to the wall clock"},
	{Name: "ELVA_TIMEOUT", Default: "10s", Help: "timeout per request to the RCU and per database operation"},
	{Name: "ELVA_MAX_METER_AGE", Default: "2m", Help: "warn when the RCU's last read of a meter is older than this (the reading is still used)"},
	{Name: "ELVA_DATABASE_URL", Help: "PostgreSQL URL, e.g. postgres://elva:pw@localhost:5432/elva (unset: print JSON to stdout instead)"},
	{Name: "ELVA_DATABASE_BUFFER", Default: "168h", Help: "how long to keep rows in memory while the database is unreachable"},
}

type options struct {
	host        string
	password    string
	meters      []string
	loc         *time.Location
	interval    time.Duration
	timeout     time.Duration
	maxMeterAge time.Duration
	dbURL       string
	dbBuffer    time.Duration
}

// parseConfig validates resolved settings.
func parseConfig(vs config.Values) (c options, err error) {
	if err := vs.CheckRequired(); err != nil {
		return c, err
	}
	c.host, c.password, c.dbURL = vs.Get("KTC_HOST"), vs.Get("KTC_PASSWORD"), vs.Get("ELVA_DATABASE_URL")
	for _, n := range strings.Split(vs.Get("KTC_METERS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			c.meters = append(c.meters, n)
		}
	}
	for name, dst := range map[string]*time.Duration{
		"ELVA_INTERVAL": &c.interval, "ELVA_TIMEOUT": &c.timeout,
		"ELVA_MAX_METER_AGE": &c.maxMeterAge, "ELVA_DATABASE_BUFFER": &c.dbBuffer,
	} {
		if *dst, err = vs.Duration(name); err != nil {
			return c, err
		}
	}
	if c.loc, err = vs.Location("KTC_TIMEZONE"); err != nil {
		return c, err
	}
	return c, nil
}
