// Package store writes measurements to PostgreSQL.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrConfig is returned when the database URL itself is invalid.
var ErrConfig = errors.New("invalid database URL")

// Schema creates the measurement table. It is idempotent and run at every startup.
// Keep it in sync with AGENTS.md.
const Schema = `CREATE TABLE IF NOT EXISTS measurement (
    time       timestamptz PRIMARY KEY,  -- scrape time, whole minutes
    el_energy  bigint,                   -- electricity meter total, Wh
    el_power   integer,                  -- electricity momentary power, W
    va_volume  bigint,                   -- water meter total, l
    va_flow    real                      -- water momentary flow, l/h
)`

// Row is one measurement. Nil fields are stored as NULL.
type Row struct {
	Time     time.Time
	ElEnergy *int64   // Wh
	ElPower  *int32   // W
	VaVolume *int64   // l
	VaFlow   *float32 // l/h
}

// Store is a connection pool to the ElVa database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to url, checks the connection and creates the schema.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	cfg.MaxConns = 2 // one writer; the second only helps while a broken connection is replaced
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if _, err := pool.Exec(ctx, Schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close closes all connections.
func (s *Store) Close() { s.pool.Close() }

// Insert writes rows in one statement. Rows whose time already exists are skipped,
// so retrying after an error or a restart can't create duplicates. It returns the
// number of rows actually inserted.
func (s *Store) Insert(ctx context.Context, rows []Row) (int64, error) {
	n := len(rows)
	ts := make([]time.Time, n)
	ee := make([]*int64, n)
	ep := make([]*int32, n)
	vv := make([]*int64, n)
	vf := make([]*float32, n)
	for i, r := range rows {
		ts[i], ee[i], ep[i], vv[i], vf[i] = r.Time, r.ElEnergy, r.ElPower, r.VaVolume, r.VaFlow
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO measurement (time, el_energy, el_power, va_volume, va_flow)
		SELECT * FROM unnest($1::timestamptz[], $2::bigint[], $3::integer[], $4::bigint[], $5::real[])
		ON CONFLICT (time) DO NOTHING`, ts, ee, ep, vv, vf)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
