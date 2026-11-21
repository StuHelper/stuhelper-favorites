package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
)

//go:embed activity_calendar_init.sql
var initializeCalendar string

//go:embed activity_calendar_switch.sql
var applyCalendar string

// RollbackActivityCalendar restores UTC activity accounting while retaining all
// activity recorded after the switch to Beijing dates. Stop writers and the
// relay before calling this method and resume them with the UTC-aware release.
func RollbackActivityCalendar(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return f.ErrUnavailable
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(726391840119)"); err != nil {
		return err
	}
	var migrated bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT FROM pg_catalog.pg_attribute
  WHERE attrelid=to_regclass('favorites.activity_retention') AND attname='shanghai_day' AND NOT attisdropped)`).Scan(&migrated); err != nil {
		return err
	}
	if migrated {
		if err := switchActivityCalendar(ctx, tx, false); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func switchActivityCalendar(ctx context.Context, tx *sql.Tx, shanghai bool) error {
	// Lock in write order so no revision can appear between reading the shift
	// ledger and publishing its conversion. DDL and rollback use the same
	// advisory lock.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE favorites.activity_buckets, favorites.activity_history, favorites.outbox
  IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	var active, initialized bool
	if err := tx.QueryRowContext(ctx, `SELECT shanghai_day,calendar_initialized
  FROM favorites.activity_retention WHERE singleton`).Scan(&active, &initialized); err != nil || active == shanghai {
		return err
	}
	if !initialized {
		var asOf time.Time
		if err := tx.QueryRowContext(ctx, "SELECT transaction_timestamp()").Scan(&asOf); err != nil {
			return err
		}
		if err := seedActivityCalendar(ctx, tx, asOf); err != nil {
			return err
		}
	}
	const utcEvent = "discovery.activity.v1"
	event, replaced, direction := utcEvent, f.ActivityEventType, int64(-1)
	if shanghai {
		event, replaced, direction = f.ActivityEventType, utcEvent, 1
	}
	if _, err := tx.ExecContext(ctx, applyCalendar, direction, event); err != nil {
		return err
	}
	var oversize bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT FROM favorites.activity_buckets WHERE first_count>1000000000)").Scan(&oversize); err != nil {
		return err
	}
	if oversize {
		return f.ErrUnavailable
	}
	// Old relays select rows using sent_at IS NULL. Keep the superseded
	// payloads while marking them ineligible for relay.
	if _, err := tx.ExecContext(ctx, `UPDATE favorites.outbox
  SET sent_at=transaction_timestamp() WHERE sent_at IS NULL AND data->>'type'=$1`, replaced); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE favorites.activity_retention
  SET shanghai_day=$1,calendar_initialized=true,revision=revision+1 WHERE singleton`, shanghai); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "SELECT pg_notify('favorites_outbox','')")
	return err
}

func seedActivityCalendar(ctx context.Context, tx *sql.Tx, asOf time.Time) error {
	if _, err := tx.ExecContext(ctx, initializeCalendar, asOf); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE favorites.activity_history h SET signed_count=b.first_count
  FROM favorites.activity_buckets b
  WHERE h.id=b.id AND h.revision=b.revision AND h.signed_count IS NULL`)
	return err
}
