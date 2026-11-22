package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// openTest connects to the database in ELVA_TEST_DATABASE_URL and starts from an
// empty measurement table. The test is skipped when the variable is unset.
// Never point it at a database with real data: the table is truncated.
func openTest(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("ELVA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ELVA_TEST_DATABASE_URL not set; skipping PostgreSQL test")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(ctx, "TRUNCATE measurement"); err != nil {
		t.Fatal(err)
	}
	return s
}

func ptr[T any](v T) *T { return &v }

func TestInsert(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC) // 02:30 CEST, before the DST change

	rows := []Row{
		{Time: t0, ElEnergy: ptr(int64(26796100)), ElPower: ptr(int32(512)), VaVolume: ptr(int64(187588)), VaFlow: ptr(float32(1.5))},
		{Time: t0.Add(time.Hour)},                               // 02:30 CET, same wall clock time: must be a separate row
		{Time: t0.Add(2 * time.Hour), ElPower: ptr(int32(400))}, // water meter missing
	}
	n, err := s.Insert(ctx, rows)
	if err != nil || n != 3 {
		t.Fatalf("Insert = %d, %v; want 3", n, err)
	}
	// Re-inserting (a retry after an error) must skip existing rows, not fail.
	n, err = s.Insert(ctx, rows[:2])
	if err != nil || n != 0 {
		t.Fatalf("re-Insert = %d, %v; want 0", n, err)
	}

	var (
		ee, vv *int64
		ep     *int32
		vf     *float32
	)
	err = s.pool.QueryRow(ctx, "SELECT el_energy, el_power, va_volume, va_flow FROM measurement WHERE time = $1", t0).Scan(&ee, &ep, &vv, &vf)
	if err != nil {
		t.Fatal(err)
	}
	if *ee != 26796100 || *ep != 512 || *vv != 187588 || *vf != 1.5 {
		t.Errorf("row = %d %d %d %v", *ee, *ep, *vv, *vf)
	}
	err = s.pool.QueryRow(ctx, "SELECT el_energy, el_power, va_volume FROM measurement WHERE time = $1", t0.Add(2*time.Hour)).Scan(&ee, &ep, &vv)
	if err != nil || ee != nil || vv != nil || *ep != 400 {
		t.Errorf("partial row: err=%v ee=%v ep=%v vv=%v", err, ee, ep, vv)
	}
}

func TestOpenBadURL(t *testing.T) {
	if _, err := Open(context.Background(), "postgres://%zz"); !errors.Is(err, ErrConfig) {
		t.Errorf("err = %v, want ErrConfig", err)
	}
}
