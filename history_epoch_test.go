package favorites_test

import (
	"context"
	"database/sql"
	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/postgres"
	"testing"
)

func TestHistoryPruneAdvancesOnceForNonemptyBatch(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	day := x.s.Now()
	for _, owner := range []string{"a", "b", "c", "d"} {
		if _, err := x.s.Add(ctx, owner, "add", f.Add{Kind: "course", TargetID: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	epoch := func() int64 {
		t.Helper()
		var v int64
		if err := x.db.QueryRow("SELECT revision FROM favorites.activity_retention WHERE singleton").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	cleaner := &postgres.Repository{DB: x.db}
	original := epoch()
	for i, want := range []int64{3, 0} {
		n, err := cleaner.PruneActivityHistory(ctx, day.AddDate(0, 0, 1), 10)
		if err != nil || n != want || epoch() != original+1 {
			t.Fatalf("prune %d: n=%d epoch=%d err=%v", i, n, epoch(), err)
		}
	}
	var retained int
	if err := x.db.QueryRow("SELECT count(*) FROM favorites.activity_history WHERE revision=4").Scan(&retained); err != nil || retained != 1 {
		t.Fatal(retained, err)
	}
}

func TestRepeatableReadKeepsSnapshotAcrossConcurrentCommit(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	tx, err := x.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var before, after string
	if err := tx.QueryRow("SELECT pg_current_snapshot()::text FROM favorites.activity_retention WHERE singleton").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.Add(ctx, "synthetic", "add", f.Add{Kind: "course", TargetID: "test"}); err != nil {
		t.Fatal(err)
	}
	var visible bool
	if err := tx.QueryRow("SELECT pg_current_snapshot()::text, EXISTS (SELECT 1 FROM favorites.activity_history)").Scan(&after, &visible); err != nil || after != before || visible {
		t.Fatal(before, after, visible, err)
	}
	var n int
	if err := x.db.QueryRow("SELECT count(*) FROM favorites.activity_history").Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
