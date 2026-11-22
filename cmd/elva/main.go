// Command elva reads electricity and water meters from a KTC RCU every minute.
//
// Configuration is read from the environment, optionally loaded from a file with
// -env-file (variables already set in the environment win). All settings are
// defined in config.go and logged at startup with secrets masked.
//
// Logs go to stderr; one JSON line per scrape goes to stdout.
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

	"github.com/andyswe/elva-ktc-2/internal/ktc"
)

// Exit codes, so a supervisor can tell why startup failed.
const (
	exitConfig      = 1
	exitUnreachable = 2
	exitAuth        = 3
	exitOther       = 4
)

func main() {
	envFile := flag.String("env-file", "", "load settings from this KEY=VALUE file (see .env.sample)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [-env-file FILE]\n\n", os.Args[0])
		flag.PrintDefaults()
		printSettingsHelp(flag.CommandLine.Output())
	}
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

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

	client := ktc.New(cfg.host, cfg.password, cfg.timeout)
	meters, err := startupCheck(ctx, log, client, cfg)
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

	out := json.NewEncoder(os.Stdout)
	log.Info("started, scraping", "interval", cfg.interval)
	for {
		// Align to wall-clock boundaries (e.g. every whole minute) for tidy time series.
		next := time.Now().Truncate(cfg.interval).Add(cfg.interval)
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return
		case <-time.After(time.Until(next)):
		}
		m, err := scrape(ctx, log, client, meters, next)
		if err != nil {
			// Transient errors (RCU rebooting, Wi-Fi hiccup) must not kill the service.
			log.Warn("scrape failed", "err", err)
			continue
		}
		if err := out.Encode(m); err != nil {
			log.Error("writing measurement", "err", err)
		}
	}
}

// startupCheck logs in, discovers meters and does one full read, so that
// configuration problems surface immediately rather than a minute later.
func startupCheck(ctx context.Context, log *slog.Logger, c *ktc.Client, cfg config) ([]ktc.MeterRef, error) {
	page, err := c.Login(ctx)
	if err != nil {
		return nil, err
	}
	log.Info("logged in to KTC", "url", c.BaseURL(), "model", page.Model, "version", page.Version, "name", page.Name)

	meters := page.Meters
	if len(cfg.meters) > 0 {
		meters = nil
		for _, want := range cfg.meters {
			i := slices.IndexFunc(page.Meters, func(m ktc.MeterRef) bool { return m.Name == want })
			if i < 0 {
				return nil, fmt.Errorf("meter %q from KTC_METERS not found on RCU (available: %s)", want, meterNames(page.Meters))
			}
			meters = append(meters, page.Meters[i])
		}
	}
	if len(meters) == 0 {
		return nil, errors.New("RCU start page lists no meters")
	}
	for _, m := range meters {
		log.Info("found meter", "name", m.Name, "object", fmt.Sprintf("MB%d", m.Pos), "value", m.Value)
	}

	if _, err := scrape(ctx, log, c, meters, time.Now()); err != nil {
		return nil, fmt.Errorf("test read: %w", err)
	}
	log.Info("test read OK")
	return meters, nil
}

// Measurement is one scrape of all meters.
type Measurement struct {
	Time   time.Time           `json:"time"`
	Meters []*ktc.MeterReading `json:"meters"`
}

// scrape logs in (the RCU session times out, and login is a single cheap request)
// and reads every meter. The RCU's M-Bus cursor is shared with other web UI users,
// so it is restored to where we found it afterwards.
func scrape(ctx context.Context, log *slog.Logger, c *ktc.Client, meters []ktc.MeterRef, at time.Time) (*Measurement, error) {
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
		m.Meters = append(m.Meters, r)
	}
	return m, nil
}

func meterNames(ms []ktc.MeterRef) string {
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Name
	}
	return strings.Join(names, ", ")
}
