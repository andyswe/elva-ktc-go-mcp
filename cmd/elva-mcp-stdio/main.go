// Command elva-mcp-stdio is an MCP server (stdio transport) that lets an AI
// assistant analyse the electricity and water data ElVa stores in PostgreSQL.
// It is read-only: sessions are read-only at the database level, and it should use
// a database role with SELECT rights only.
//
// A client starts it as a subprocess, typically over SSH on the database host:
//
//	ssh host /opt/elva/bin/elva-mcp-stdio -env-file /opt/elva/mcp.env
//
// stdout carries the MCP protocol; logs go to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // ELVA_TIMEZONE must resolve even where the OS has no zone database

	"github.com/andyswe/elva-ktc-go-mcp/internal/config"
	"github.com/andyswe/elva-ktc-go-mcp/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is set at build time by scripts/build.sh.
var version = "dev"

// Exit codes, as for elva.
const (
	exitConfig   = 1
	exitDatabase = 5
)

var settings = []config.Setting{
	{Name: "ELVA_DATABASE_URL", Required: true, Help: "PostgreSQL URL of a read-only role, e.g. postgres://elva_ro:pw@localhost:5432/elva"},
	{Name: "ELVA_TIMEZONE", Default: "Europe/Stockholm", Help: "time zone for calendar periods (days, months) and output"},
	{Name: "ELVA_QUERY_TIMEOUT", Default: "30s", Help: "maximum run time of one database statement"},
	{Name: "ELVA_QUERY_MAX_ROWS", Default: "1000", Help: "maximum rows returned by the query tool"},
}

func main() {
	envFile := flag.String("env-file", "", "load settings from this KEY=VALUE file (see deploy/mcp.env.sample)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	check := flag.Bool("check", false, "connect to the database, show what data exists, then exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [-env-file FILE] [-check] [-version]\n\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "MCP server (stdio) for ElVa data. Started by an MCP client, not by hand.\n\n")
		flag.PrintDefaults()
		config.PrintHelp(flag.CommandLine.Output(), settings)
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("elva-mcp-stdio starting", "version", version, "check_only", *check)
	vs, err := config.Load(log, settings, *envFile)
	if err != nil {
		log.Error("cannot load env file", "err", err)
		os.Exit(exitConfig)
	}
	s := &server{now: time.Now}
	timeout, err := parse(vs, s)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(exitConfig)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	octx, cancel := context.WithTimeout(ctx, 10*time.Second)
	s.db, err = store.OpenReader(octx, vs.Get("ELVA_DATABASE_URL"), s.loc, timeout)
	cancel()
	if errors.Is(err, store.ErrConfig) {
		log.Error("invalid ELVA_DATABASE_URL", "err", err)
		os.Exit(exitConfig)
	}
	if err != nil {
		log.Error("cannot connect to database: check ELVA_DATABASE_URL and that the role may read table measurement", "err", err)
		os.Exit(exitDatabase)
	}
	defer s.db.Close()
	log.Info("connected to database (read-only)")

	if *check {
		info, err := s.dataInfo(ctx, dataInfoIn{})
		if err != nil {
			log.Error("reading data info", "err", err)
			os.Exit(exitDatabase)
		}
		log.Info("data", "rows", info.Rows, "first", deref(info.First), "last", deref(info.Last), "gaps", info.Gaps)
		log.Info("check passed")
		return
	}

	if err := newServer(log, s).Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Error("MCP session ended with error", "err", err)
		os.Exit(1)
	}
	log.Info("MCP session ended")
}

// parse validates the settings into s and returns the statement timeout.
func parse(vs config.Values, s *server) (time.Duration, error) {
	if err := vs.CheckRequired(); err != nil {
		return 0, err
	}
	var err error
	if s.loc, err = vs.Location("ELVA_TIMEZONE"); err != nil {
		return 0, err
	}
	if s.maxRows, err = strconv.Atoi(vs.Get("ELVA_QUERY_MAX_ROWS")); err != nil || s.maxRows < 1 {
		return 0, fmt.Errorf("ELVA_QUERY_MAX_ROWS=%q: must be a positive number", vs.Get("ELVA_QUERY_MAX_ROWS"))
	}
	return vs.Duration("ELVA_QUERY_TIMEOUT")
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
