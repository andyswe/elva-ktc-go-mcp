package main

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
)

// setting describes one configuration variable. Every setting must be listed here:
// the table drives loading, the startup log and -help, so a new setting is
// documented and logged automatically.
type setting struct {
	name     string
	def      string // default value; empty means none
	required bool
	secret   bool // never logged; names containing PASSWORD/SECRET/TOKEN are treated as secret too
	help     string
}

var settings = []setting{
	{name: "KTC_HOST", required: true, help: "host or host:port of the KTC RCU, e.g. 192.168.1.100"},
	{name: "KTC_PASSWORD", required: true, secret: true, help: "RCU web UI login password"},
	{name: "KTC_METERS", help: "comma-separated meter names to read, e.g. EL-01,VV-01 (default: all meters on the RCU)"},
	{name: "KTC_TIMEZONE", def: "Europe/Stockholm", help: "time zone of the RCU clock (its timestamps carry none)"},
	{name: "ELVA_INTERVAL", def: "1m", help: "scrape interval, aligned to the wall clock"},
	{name: "ELVA_TIMEOUT", def: "10s", help: "timeout per HTTP request to the RCU"},
	{name: "ELVA_MAX_METER_AGE", def: "2m", help: "warn when the RCU's last read of a meter is older than this (the reading is still used)"},
}

type config struct {
	host        string
	password    string
	meters      []string
	loc         *time.Location
	interval    time.Duration
	timeout     time.Duration
	maxMeterAge time.Duration
}

// resolved is a setting's effective value and where it came from.
type resolved struct {
	setting
	value  string
	source string // "environment", "env file", "default" or "none"
}

// resolveSettings looks up every setting. fromFile holds the names set by -env-file.
func resolveSettings(fromFile map[string]bool) []resolved {
	out := make([]resolved, len(settings))
	for i, s := range settings {
		r := resolved{setting: s, source: "none"}
		if v, ok := os.LookupEnv(s.name); ok && v != "" {
			r.value, r.source = v, "environment"
			if fromFile[s.name] {
				r.source = "env file"
			}
		} else if s.def != "" {
			r.value, r.source = s.def, "default"
		}
		out[i] = r
	}
	return out
}

// logSettings logs every setting with secrets masked.
func logSettings(log *slog.Logger, rs []resolved) {
	for _, r := range rs {
		log.Info("setting", "name", r.name, "value", r.display(), "source", r.source)
	}
}

func (r resolved) display() string {
	switch {
	case r.value == "":
		return "(not set)"
	case r.secret || looksSecret(r.name):
		return "*****"
	}
	// Connection strings like postgres://user:pw@host/db carry a password.
	if u, err := url.Parse(r.value); err == nil && u.User != nil {
		if _, ok := u.User.Password(); ok {
			return u.Redacted()
		}
	}
	return r.value
}

func looksSecret(name string) bool {
	for _, w := range []string{"PASSWORD", "SECRET", "TOKEN"} {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

// parseConfig validates resolved settings into a config.
func parseConfig(rs []resolved) (config, error) {
	v := map[string]string{}
	var missing []string
	for _, r := range rs {
		v[r.name] = r.value
		if r.required && r.value == "" {
			missing = append(missing, r.name)
		}
	}
	var cfg config
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required setting(s): %s", strings.Join(missing, ", "))
	}
	cfg.host, cfg.password = v["KTC_HOST"], v["KTC_PASSWORD"]
	for _, n := range strings.Split(v["KTC_METERS"], ",") {
		if n = strings.TrimSpace(n); n != "" {
			cfg.meters = append(cfg.meters, n)
		}
	}
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{{"ELVA_INTERVAL", &cfg.interval}, {"ELVA_TIMEOUT", &cfg.timeout}, {"ELVA_MAX_METER_AGE", &cfg.maxMeterAge}} {
		dur, err := time.ParseDuration(v[d.name])
		if err != nil || dur <= 0 {
			return cfg, fmt.Errorf("%s=%q: must be a positive duration like 1m or 30s", d.name, v[d.name])
		}
		*d.dst = dur
	}
	loc, err := time.LoadLocation(v["KTC_TIMEZONE"])
	if err != nil {
		return cfg, fmt.Errorf("KTC_TIMEZONE=%q: %v", v["KTC_TIMEZONE"], err)
	}
	cfg.loc = loc
	return cfg, nil
}

// loadEnvFile sets KEY=VALUE lines from path, skipping blanks and # comments, and
// returns the names it set. Variables already present in the environment win.
func loadEnvFile(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	set := map[string]bool{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
			set[k] = true
		}
	}
	return set, sc.Err()
}

// printSettingsHelp writes the settings table for -help.
func printSettingsHelp(w io.Writer) {
	fmt.Fprintln(w, "\nSettings (environment variables; also accepted in the -env-file file):")
	for _, s := range settings {
		extra := ""
		switch {
		case s.required:
			extra = " (required)"
		case s.def != "":
			extra = " (default " + s.def + ")"
		}
		fmt.Fprintf(w, "  %-14s %s%s\n", s.name, s.help, extra)
	}
}
