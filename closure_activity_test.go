package favorites_test

import (
	"context"
	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestClosureUsesServiceClockAndOrderedActivityIDs(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	day := time.Date(2024, 2, 29, 23, 59, 59, 0, time.UTC)
	x.s.Now = func() time.Time { return day }
	for _, id := range []string{"30", "3a"} {
		if _, err := x.s.Add(ctx, "owner", id, f.Add{Kind: "course", TargetID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.s.CloseOwner(ctx, "owner", "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	rows, err := x.db.Query(`SELECT data->>'subject', (data->>'time')::timestamptz FROM favorites.outbox ORDER BY seq OFFSET 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for _, target := range []string{"3a", "30"} {
		var id string
		var at time.Time
		if !rows.Next() {
			t.Fatal("missing closure event", rows.Err())
		}
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatal(err)
		}
		want := f.ActivityID("course", target, day)
		if id != want || !at.Equal(day) {
			t.Errorf("got %s %s, want %s %s", id, at, want, day)
		}
		d, err := x.repo.ReadActivity(ctx, want)
		if err != nil || d.Visible || d.Revision != 2 || d.Metrics["favorites"] != 0 {
			t.Errorf("wrong tombstone: %+v %v", d, err)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatal("unexpected remaining closure events", rows.Err())
	}
}

type statementCounter struct{ n atomic.Int32 }

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (*statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestClosureBatchesDatabaseRoundTrips(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	folders, err := x.s.ListFolders(ctx, "batch", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	// Import 300 items sharing a long prefix so cursor batches cross base64 line boundaries.
	_, err = x.db.Exec(`INSERT INTO favorites.items(owner_key,id,kind,target_id,canonical_target_id,title,folder_id,revision,created_at)
   SELECT 'batch',i::text,'course',target,target,'sample',$1,1,now() FROM
   (SELECT i,repeat('n',64)||i::text AS target FROM generate_series(1,300)i) fixture`, folders.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(os.Getenv("STUHELPER_FAVORITES_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	if err := x.repo.DB.QueryRow("SELECT current_database(),current_user").Scan(&cfg.Database, &cfg.User); err != nil {
		t.Fatal(err)
	}
	count := new(statementCounter)
	cfg.Tracer = count
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	svc := f.Service{Repo: &postgres.Repository{DB: db}, Now: x.s.Now}
	if err := svc.CloseOwner(ctx, "batch", "22222222-2222-4222-8222-222222222222"); err != nil {
		t.Fatal(err)
	}
	if n := count.n.Load(); n > 30 {
		t.Errorf("300 items took %d statements", n)
	} else {
		t.Logf("300 items took %d statements", n)
	}
	rows, err := x.db.Query("SELECT data->>'subject' FROM favorites.outbox ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	previous := ""
	n := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id <= previous {
			t.Fatal("out-of-order closure event", id, previous)
		}
		previous = id
		n++
	}
	if err := rows.Err(); err != nil || n != 300 {
		t.Fatal(n, err)
	}
}
