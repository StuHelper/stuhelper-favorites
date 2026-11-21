package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
)

// legacyActivity writes the same UTC bucket, clamped snapshot and envelope as
// the old runtime. The retained ledger trigger also sees a rolled-back writer.
func legacyActivity(t *testing.T, db *sql.DB, target, day string, revision, signed int64, at time.Time, envelope bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO favorites.activity_buckets(id,kind,target_id,day,first_count,revision,visible)
  VALUES('course/' || translate(encode(convert_to($1,'UTF8'),'base64'),'+/=','-_') || '/' || $2,
      'course',$1,$2::text::date,$4,$3,$4::bigint>0)
  ON CONFLICT(id) DO UPDATE SET first_count=EXCLUDED.first_count,revision=EXCLUDED.revision
  RETURNING id`, target, day, revision, signed).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.ExecContext(ctx, `WITH snapshot AS (
    SELECT jsonb_build_object('source','favorites','id',$7::text,'day',$2::text,
        'target',jsonb_build_object('source','catalog','kind','course','id',$1::text),
        'revision',$3::bigint,'visible',$4::bigint>0,'metrics',jsonb_build_object('favorites',greatest($4::bigint,0))) AS data
), saved AS (
    UPDATE favorites.activity_buckets SET visible=$4>0,data=snapshot.data
    FROM snapshot WHERE id=$7 RETURNING id,revision,day,favorites.activity_buckets.data
), history AS (
    INSERT INTO favorites.activity_history(id,revision,day,data) SELECT id,revision,day,data FROM saved
    RETURNING id,revision,data
), event AS (
    SELECT id,data,'activity:' || encode(sha256(convert_to(id,'UTF8')),'hex') || ':' || revision AS event_id
    FROM history
)
INSERT INTO favorites.outbox(id,data)
SELECT event_id,jsonb_build_object('specversion','1.0','id',event_id,'source','favorites',
    'type','discovery.activity.v1','subject',id,'time',$5::text,'data',data) FROM event WHERE $6::boolean`,
		target, day, revision, signed, at.Format(time.RFC3339Nano), envelope, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestActivityCalendarMigrationExactApproximateAndRollback(t *testing.T) {
	repo, cleaner, db := activityDatabase(t)
	ctx := context.Background()
	midnightUTC := time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)
	exec := func(query string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	assertBucket := func(target, day string, wantSigned, wantRevision int64) {
		t.Helper()
		var signed, revision, published int64
		var visible bool
		if err := db.QueryRow(`SELECT first_count,revision,(data->'metrics'->>'favorites')::bigint,visible
      FROM favorites.activity_buckets WHERE kind='course' AND target_id=$1 AND day=$2::text::date`, target, day).
			Scan(&signed, &revision, &published, &visible); err != nil {
			t.Fatal(err)
		}
		if signed != wantSigned || revision != wantRevision || published != max(0, wantSigned) || visible != (wantSigned > 0) {
			t.Fatalf("bucket %s/%s signed=%d revision=%d published=%d visible=%t", target, day, signed, revision, published, visible)
		}
	}
	assertPending := func(eventType string, count int) {
		t.Helper()
		rows, err := repo.PendingOutbox(ctx, 100, nil)
		if err != nil || len(rows) != count {
			t.Fatalf("pending=%d, want %d: %v", len(rows), count, err)
		}
		for _, row := range rows {
			if row.Type != eventType {
				t.Fatalf("pending %s, want %s", row.Type, eventType)
			}
		}
	}
	if err := RollbackActivityCalendar(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Recreate old daily snapshots lacking the cutover schema's private data.
	exec(`ALTER TABLE favorites.activity_history DISABLE TRIGGER capture_activity_signed_count;
      ALTER TABLE favorites.outbox DISABLE TRIGGER track_activity_calendar;
      UPDATE favorites.activity_retention SET calendar_initialized=false WHERE singleton`)
	legacyActivity(t, db, "exact", "2026-10-31", 1, 1, midnightUTC.Add(-time.Nanosecond), true)
	legacyActivity(t, db, "exact", "2026-10-31", 2, 2, midnightUTC, true)
	legacyActivity(t, db, "exact", "2026-10-31", 3, -2, midnightUTC.Add(time.Hour), true)
	legacyActivity(t, db, "opaque", "2026-10-31", 8, 8, midnightUTC, false)
	legacyActivity(t, db, "with-item", "2026-10-31", 1, 1, midnightUTC, false)
	exec(`INSERT INTO favorites.owners(owner_key) VALUES('legacy-owner');
      INSERT INTO favorites.folders(owner_key,id,name,is_default,revision) VALUES('legacy-owner','f','',true,1);
      INSERT INTO favorites.items(owner_key,id,kind,target_id,canonical_target_id,title,folder_id,revision,created_at)
        VALUES('legacy-owner','i','course','with-item','with-item','','f',1,'2026-10-31 16:00:00+00');
      ALTER TABLE favorites.activity_history ENABLE TRIGGER capture_activity_signed_count;
      ALTER TABLE favorites.outbox ENABLE TRIGGER track_activity_calendar`)
	exec(`ALTER TABLE favorites.outbox ADD CONSTRAINT fail_switch
        CHECK(data->>'type'<>'discovery.activity.v2') NOT VALID`)
	if err := Migrate(ctx, db); err == nil {
		t.Fatal("migration accepted a rejected replacement event")
	}
	assertBucket("exact", "2026-10-31", -2, 3)
	assertPending("discovery.activity.v1", 3)
	exec("ALTER TABLE favorites.outbox DROP CONSTRAINT fail_switch")
	for range 2 {
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		assertBucket("exact", "2026-10-31", 1, 4)
		assertBucket("exact", "2026-11-01", -3, 1)
		assertBucket("opaque", "2026-10-31", 5, 9)
		assertBucket("opaque", "2026-11-01", 3, 1)
		assertBucket("with-item", "2026-10-31", 0, 2)
		assertBucket("with-item", "2026-11-01", 1, 1)
		assertPending(f.ActivityEventType, 6)
	}
	// Add before a Beijing date's end, then prune its history before rollback.
	tx, err := repo.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := (&transaction{ctx: ctx, tx: tx}).changeActivity("course", "exact", 1, midnightUTC.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertBucket("exact", "2026-11-01", -2, 2)
	if n, err := cleaner.PruneActivityHistory(ctx, f.ActivityDayStart(midnightUTC.AddDate(0, 0, 1)), 100); err != nil || n == 0 {
		t.Fatalf("pruned=%d err=%v", n, err)
	}
	for range 2 {
		if err := RollbackActivityCalendar(ctx, db); err != nil {
			t.Fatal(err)
		}
		assertBucket("exact", "2026-10-31", -1, 5)
		assertBucket("exact", "2026-11-01", 0, 3)
		assertBucket("opaque", "2026-10-31", 8, 10)
		assertBucket("opaque", "2026-11-01", 0, 2)
		assertPending("discovery.activity.v1", 6)
	}

	// The deployed UTC binary can continue writing, and another upgrade
	// transports those changes precisely even with missing legacy history.
	legacyActivity(t, repo.DB, "exact", "2026-10-31", 6, 0, midnightUTC.Add(2*time.Hour), true)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertBucket("exact", "2026-10-31", 1, 7)
	assertBucket("exact", "2026-11-01", -1, 4)
	assertPending(f.ActivityEventType, 6)
}

func TestActivityCalendarAllocatesOnlyElapsedTime(t *testing.T) {
	_, _, db := activityDatabase(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	legacyActivity(t, db, "opaque", "2026-10-31", 18, 18, day, false)
	for _, tc := range []struct {
		name  string
		asOf  time.Time
		shift int64
	}{
		{"before UTC 16", day.Add(16*time.Hour - time.Microsecond), 0},
		{"at UTC 16", day.Add(16 * time.Hour), 0},
		{"at UTC 20", day.Add(20 * time.Hour), 4},
		{"after full UTC day", day.AddDate(0, 0, 1), 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := seedActivityCalendar(ctx, tx, tc.asOf); err != nil {
				t.Fatal(err)
			}
			var shift int64
			if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(amount),0)
      FROM favorites.activity_calendar_shifts WHERE kind='course' AND target_id='opaque'`).Scan(&shift); err != nil {
				t.Fatal(err)
			}
			if shift != tc.shift {
				t.Fatalf("shift=%d, want %d", shift, tc.shift)
			}
		})
	}
}
