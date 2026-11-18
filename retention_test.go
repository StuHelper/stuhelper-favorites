package favorites_test

import (
	"context"
	"database/sql"
	"errors"
	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/postgres"
	"os"
	"testing"
)

func TestReplayExpiryAndBoundedRetention(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	first, err := x.s.CreateFolder(ctx, "sample", "key", "old")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the previous schema with a live receipt, then migrate twice.
	if _, err := x.db.Exec("ALTER TABLE favorites.requests DROP COLUMN IF EXISTS created_at"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := postgres.Migrate(ctx, x.db); err != nil {
			t.Fatal(err)
		}
	}
	replayed, err := x.s.CreateFolder(ctx, "sample", "key", "old")
	if err != nil || replayed.ID != first.ID {
		t.Fatal(replayed, err)
	}
	if _, err := x.s.CreateFolder(ctx, "sample", "key", "new"); !errors.Is(err, f.ErrConflict) {
		t.Fatal(err)
	}
	exec := func(query string) {
		t.Helper()
		if _, err := x.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE favorites.requests SET created_at=now()-interval '25 hours'")
	second, err := x.s.CreateFolder(ctx, "sample", "key", "new")
	if err != nil || second.ID == first.ID {
		t.Fatal(second, err)
	}
	exec(`INSERT INTO favorites.requests(owner_key,key,digest,result,created_at)
 SELECT 'expired',i::text,'digest','{}',now()-interval '25 hours' FROM generate_series(1,3) i;
 INSERT INTO favorites.outbox(id,data,sent_at) SELECT 'old'||i,'{}',now()-interval '91 days' FROM generate_series(1,3) i;
 INSERT INTO favorites.outbox(id,data,sent_at) VALUES('pending','{}',NULL),('recent','{}',now());
 INSERT INTO favorites.events(owner_key,kind,operation,created_at) SELECT 'expired','test',i::text,now()-interval '13 months' FROM generate_series(1,3) i`)
	var database string
	if err := x.db.QueryRow("SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	role := database + "_cleaner"
	exec("CREATE ROLE " + role + " LOGIN")
	t.Cleanup(func() {
		if _, err := x.db.Exec("DROP OWNED BY " + role + "; DROP ROLE " + role); err != nil {
			t.Error(err)
		}
	})
	exec("GRANT USAGE ON SCHEMA favorites TO " + role + "; GRANT SELECT,DELETE ON favorites.events,favorites.requests,favorites.outbox TO " + role)
	db, err := sql.Open("pgx", os.Getenv("STUHELPER_FAVORITES_TEST_DSN")+" dbname="+database+" user="+role)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cleaner := &postgres.Repository{DB: db}

	for _, limit := range []int{0, 1001} {
		if _, err := cleaner.PruneExpired(ctx, limit); !errors.Is(err, f.ErrInput) {
			t.Fatal(err)
		}
	}
	// The runtime lacks audit DELETE. Failure in the last class rolls all deletions back.
	if n, err := x.repo.PruneExpired(ctx, 2); n != 0 || !errors.Is(err, f.ErrUnavailable) {
		t.Fatal(n, err)
	}
	for _, want := range []int64{6, 3, 0} {
		n, err := cleaner.PruneExpired(ctx, 2)
		if err != nil || n != want {
			t.Fatal(n, err)
		}
	}
	for _, q := range []struct {
		sql string
		n   int
	}{
		{"SELECT count(*) FROM favorites.requests", 1}, {"SELECT count(*) FROM favorites.events", 2},
		{"SELECT count(*) FROM favorites.outbox WHERE id IN ('pending','recent')", 2},
		{"SELECT count(*) FROM favorites.outbox", 2}, {"SELECT count(*) FROM favorites.folders WHERE NOT is_default", 2},
	} {
		var n int
		if err := x.db.QueryRow(q.sql).Scan(&n); err != nil || n != q.n {
			t.Fatal(q, n, err)
		}
	}
	replayed, err = x.s.CreateFolder(ctx, "sample", "key", "new")
	if err != nil || replayed.ID != second.ID {
		t.Fatal(replayed, err)
	}
}
