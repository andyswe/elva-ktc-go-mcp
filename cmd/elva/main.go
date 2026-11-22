// Command elva reads electricity and water meters from a KTC RCU every minute and
// stores them in PostgreSQL (or prints them as JSON lines when no database is set).
//
// Configuration is read from the environment, optionally loaded from a file with
// -env-file (variables already set in the environment win). All settings are
// defined in config.go and logged at startup with secrets masked.
//
// Logs go to stderr.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // KTC_TIMEZONE must resolve even where the OS has no zone database

	"github.com/andyswe/elva-ktc-2/internal/ktc"
	"github.com/andyswe/elva-ktc-2/internal/store"
)

// version is set at build time by scripts/build.sh (-ldflags "-X main.version=...").
var version = "dev"

// Exit codes, so a supervisor can tell why startup failed.
const (
	exitConfig      = 1
	exitUnreachable = 2
	exitAuth        = 3
	exitOther       = 4
	exitDatabase    = 5
)

func main() {
	envFile := flag.String("env-file", "", "load settings from this KEY=VALUE file (see .env.sample)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	check := flag.Bool("check", false, "run the startup checks (config, database, RCU test read), then exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [-env-file FILE] [-check] [-version]\n\n", os.Args[0])
		flag.PrintDefaults()
		printSettingsHelp(flag.CommandLine.Output())
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("ElVa starting", "version", version, "check_only", *check)

	var fromFile map[string]bool
	if *envFile != "" {
		var err error
		if fromFile, err = loadEnvFile(*envFile); err != nil {
			log.Error("cannot load env file", "err", err)
			os.Exit(exitConfig)
		}
		log.Info("loaded env file", "path", *envFile)
	}
	rs := resolveSettings(fromFile)
	logSettings(log, rs)
	cfg, err := parseConfig(rs)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(exitConfig)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var db *store.Store
	if cfg.dbURL != "" {
		octx, cancel := context.WithTimeout(ctx, cfg.timeout)
		db, err = store.Open(octx, cfg.dbURL)
		cancel()
		if errors.Is(err, store.ErrConfig) {
			log.Error("invalid ELVA_DATABASE_URL", "err", err)
			os.Exit(exitConfig)
		}
		if err != nil {
			log.Error("cannot connect to database: check ELVA_DATABASE_URL and that PostgreSQL is running", "err", err)
			os.Exit(exitDatabase)
		}
		defer db.Close()
		log.Info("connected to database, table measurement ready")
		lctx, cancel := context.WithTimeout(ctx, cfg.timeout)
		last, err := db.Latest(lctx)
		cancel()
		switch {
		case err != nil:
			log.Warn("cannot read latest stored row", "err", err)
		case last.IsZero():
			log.Info("database has no rows yet")
		default:
			age := time.Since(last)
			log.Info("last stored row", "time", last, "age", age.Round(time.Second), "age_seconds", int(age.Seconds()))
		}
	} else {
		log.Info("ELVA_DATABASE_URL not set: printing measurements as JSON to stdout")
	}

	client := ktc.New(cfg.host, cfg.password, cfg.timeout, cfg.loc)
	meters, first, err := startupCheck(ctx, log, client, cfg)
	if err != nil {
		switch {
		case errors.Is(err, ktc.ErrUnreachable):
			log.Error("cannot connect to KTC: check KTC_HOST and that the RCU is on the network", "url", client.BaseURL(), "err", err)
			os.Exit(exitUnreachable)
		case errors.Is(err, ktc.ErrAuth):
			log.Error("cannot log in to KTC: check KTC_PASSWORD", "url", client.BaseURL(), "err", err)
			os.Exit(exitAuth)
		default:
			log.Error("KTC startup check failed", "url", client.BaseURL(), "err", err)
			os.Exit(exitOther)
		}
	}
	if db != nil {
		if _, err := toRow(first); err != nil {
			log.Error("cannot map meters to database columns", "err", err)
			os.Exit(exitOther)
		}
		log.Info("storing", "columns", storedColumns(first))
	}
	if *check {
		log.Info("check passed")
		return
	}

	out := json.NewEncoder(os.Stdout)
	buf := &buffer{max: max(1, int(cfg.dbBuffer/cfg.interval))}
	dbDown := false
	log.Info("started, scraping", "interval", cfg.interval)
	for {
		// Align to wall-clock boundaries (e.g. every whole minute) for tidy time series.
		next := time.Now().Truncate(cfg.interval).Add(cfg.interval)
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			if db != nil && len(buf.rows) > 0 {
				fctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
				if _, err := buf.flush(fctx, db.Insert); err != nil {
					log.Error("database unreachable at shutdown: buffered rows lost", "rows", len(buf.rows), "err", err)
				}
				cancel()
			}
			return
		case <-time.After(time.Until(next)):
		}
		m, err := scrape(ctx, log, client, meters, next, cfg.maxMeterAge)
		if err != nil {
			// Transient errors (RCU rebooting, Wi-Fi hiccup) must not kill the service.
			log.Warn("scrape failed", "err", err)
			continue
		}
		if db == nil {
			if err := out.Encode(m); err != nil {
				log.Error("writing measurement", "err", err)
			}
			continue
		}
		row, err := toRow(m)
		if err != nil {
			log.Warn("cannot map measurement to database columns", "err", err)
			continue
		}
		if dropped := buf.add(row); dropped > 0 {
			log.Error("database buffer full: dropped oldest rows", "dropped", dropped, "buffer", cfg.dbBuffer)
		}
		wctx, cancel := context.WithTimeout(ctx, cfg.timeout)
		written, err := buf.flush(wctx, db.Insert)
		cancel()
		switch {
		case err != nil:
			// Keep the rows and retry on the next tick; totals stay correct across gaps anyway.
			log.Warn("database write failed, buffering", "buffered", len(buf.rows), "err", err)
			dbDown = true
		case dbDown:
			log.Info("database reachable again, buffered rows written", "rows", written)
			dbDown = false
		}
	}
}

// startupCheck logs in, discovers meters and does one full read, so that
// configuration problems surface immediately rather than a minute later.
// It returns the meters to read and the test measurement.
func startupCheck(ctx context.Context, log *slog.Logger, c *ktc.Client, cfg config) ([]ktc.MeterRef, *Measurement, error) {
	page, err := c.Login(ctx)
	if err != nil {
		return nil, nil, err
	}
	log.Info("logged in to KTC", "url", c.BaseURL(), "model", page.Model, "version", page.Version, "name", page.Name)

	meters := page.Meters
	if len(cfg.meters) > 0 {
		meters = nil
		for _, want := range cfg.meters {
			i := slices.IndexFunc(page.Meters, func(m ktc.MeterRef) bool { return m.Name == want })
			if i < 0 {
				return nil, nil, fmt.Errorf("meter %q from KTC_METERS not found on RCU (available: %s)", want, meterNames(page.Meters))
			}
			meters = append(meters, page.Meters[i])
		}
	}
	if len(meters) == 0 {
		return nil, nil, errors.New("RCU start page lists no meters")
	}
	for _, m := range meters {
		log.Info("found meter", "name", m.Name, "object", fmt.Sprintf("MB%d", m.Pos), "value", m.Value)
	}

	first, err := scrape(ctx, log, c, meters, time.Now(), cfg.maxMeterAge)
	if err != nil {
		return nil, nil, fmt.Errorf("test read: %w", err)
	}
	log.Info("test read OK")
	return meters, first, nil
}

// Measurement is one scrape of all meters.
type Measurement struct {
	Time   time.Time           `json:"time"`
	Meters []*ktc.MeterReading `json:"meters"`
}

// scrape logs in (the RCU session times out, and login is a single cheap request)
// and reads every meter. The RCU's M-Bus cursor is shared with other web UI users,
// so it is restored to where we found it afterwards.
func scrape(ctx context.Context, log *slog.Logger, c *ktc.Client, meters []ktc.MeterRef, at time.Time, maxMeterAge time.Duration) (*Measurement, error) {
	if _, err := c.Login(ctx); err != nil {
		return nil, err
	}
	orig, err := c.Cursor(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Use a fresh context so the cursor is restored even when shutting down.
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.SetCursor(rctx, orig); err != nil {
			log.Warn("could not restore RCU M-Bus cursor", "pos", orig, "err", err)
		}
	}()

	m := &Measurement{Time: at.Truncate(time.Second)}
	for _, ref := range meters {
		r, err := c.ReadMeter(ctx, ref.Pos)
		if err != nil {
			return nil, err
		}
		if r.Name != ref.Name {
			return nil, fmt.Errorf("MB%d: expected meter %q, RCU reports %q", ref.Pos, ref.Name, r.Name)
		}
		if r.CommStatus != "OK" {
			log.Warn("RCU reports meter communication problem", "meter", r.Name, "status", r.CommStatus, "meter_time", r.MeterTime)
		}
		if problem := meterAgeProblem(time.Now(), r.MeterTime, maxMeterAge); problem != "" {
			// The reading is still used: totals stay correct, only the rate may be outdated.
			log.Warn(problem, "meter", r.Name, "meter_time", r.MeterTime, "max_age", maxMeterAge)
		}
		m.Meters = append(m.Meters, r)
	}
	return m, nil
}

// meterAgeProblem checks how long ago the RCU last read a meter. The RCU reads every
// meter about once a minute, so a healthy reading is 0-60 s old. It returns "" when fine.
func meterAgeProblem(now, meterTime time.Time, maxAge time.Duration) string {
	age := now.Sub(meterTime)
	switch {
	case meterTime.IsZero():
		return "RCU reports no time for its last read of the meter"
	case age > maxAge:
		return fmt.Sprintf("stale meter reading: RCU last read the meter %s ago (if constant, check KTC_TIMEZONE and the RCU clock)", age.Round(time.Second))
	case age < -maxAge:
		return fmt.Sprintf("meter reading is %s in the future: check KTC_TIMEZONE and the RCU clock", (-age).Round(time.Second))
	}
	return ""
}

func meterNames(ms []ktc.MeterRef) string {
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Name
	}
	return strings.Join(names, ", ")
}
