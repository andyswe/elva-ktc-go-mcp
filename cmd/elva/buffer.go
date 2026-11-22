package main

import (
	"context"

	"github.com/andyswe/elva-ktc-2/internal/store"
)

// buffer holds rows not yet written to the database, so a database outage loses
// nothing as long as it is shorter than the buffer (ELVA_DATABASE_BUFFER). It lives in
// memory: restarting ElVa during an outage loses the buffered rows.
type buffer struct {
	rows []store.Row
	max  int
}

// add queues a row and returns how many of the oldest rows were dropped to make room.
func (b *buffer) add(r store.Row) (dropped int) {
	if len(b.rows) >= b.max {
		dropped = len(b.rows) - b.max + 1
		b.rows = b.rows[dropped:]
	}
	b.rows = append(b.rows, r)
	return dropped
}

// flushChunk bounds the size of a single INSERT after a long outage.
const flushChunk = 1000

// flush writes buffered rows, oldest first, and removes the ones written. It stops
// at the first error, leaving the rest queued for the next attempt.
func (b *buffer) flush(ctx context.Context, insert func(context.Context, []store.Row) (int64, error)) (written int, err error) {
	for len(b.rows) > 0 {
		n := min(len(b.rows), flushChunk)
		if _, err := insert(ctx, b.rows[:n]); err != nil {
			return written, err
		}
		b.rows = b.rows[n:]
		written += n
	}
	b.rows = nil // release the backing array after a long outage
	return written, nil
}
