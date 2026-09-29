package hstore

import (
	"errors"
	"fmt"
)

// BalancePollRow is an observation of exchange cash. It is deliberately
// separate from fills-derived PNL and from the capital model.
type BalancePollRow struct {
	TsMs         int64
	RunID        string
	BalanceCents int64
}

// RecordBalancePoll submits one complete balance observation to the sole
// writer. A failed REST read has no row to submit.
func (s *Store) RecordBalancePoll(h RunHandle, tsMs, balanceCents int64) (Receipt, error) {
	if !h.Valid() {
		return Receipt{}, errors.New("balance_poll requires a committed run")
	}
	if tsMs <= 0 {
		return Receipt{}, fmt.Errorf("balance_poll timestamp %d is not positive", tsMs)
	}
	return s.submit(&submission{kind: KindBalancePoll, balance: BalancePollRow{
		TsMs: tsMs, RunID: h.RunID(), BalanceCents: balanceCents,
	}})
}

// The optional method keeps existing synthetic backends focused on the audit
// records they exercise. Production always uses sqliteBackend.
func recordBalancePoll(back backend, row BalancePollRow) error {
	w, ok := back.(interface{ recordBalancePoll(BalancePollRow) error })
	if !ok {
		return permanent("balance_poll backend has no writer")
	}
	return w.recordBalancePoll(row)
}

func (b *sqliteBackend) recordBalancePoll(row BalancePollRow) error {
	_, err := b.db.Exec(`INSERT INTO balance_poll(ts_ms, run_id, balance_cents)
		VALUES (?, ?, ?) ON CONFLICT(ts_ms) DO NOTHING`, row.TsMs, row.RunID,
		row.BalanceCents)
	if err != nil {
		return fmt.Errorf("record balance_poll: %w", err)
	}
	return nil
}

// BalancePolls returns the observed series for payout review. The values have
// no path into H-HALT-5's fills-derived PNL.
func (r *Reader) BalancePolls() ([]BalancePollRow, error) {
	rows, err := r.db.Query(`SELECT ts_ms, run_id, balance_cents
		FROM balance_poll ORDER BY ts_ms`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BalancePollRow
	for rows.Next() {
		var row BalancePollRow
		if err := rows.Scan(&row.TsMs, &row.RunID, &row.BalanceCents); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// confidence: high
