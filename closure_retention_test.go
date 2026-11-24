package favorites_test

import (
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

func TestClosedOwnerExpiresOnlyAfterRecordedCompletion(t *testing.T) {
	f := setup(t)
	runtime, ownerDB := f.repo, f.db
	ctx := t.Context()
	var role string
	if err := runtime.DB.QueryRowContext(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerDB.ExecContext(ctx, "GRANT EXECUTE ON FUNCTION favorites.mark_closure_completed(text,text,timestamptz),favorites.prune_closure_markers(integer) TO "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"expired", "current", "incomplete"} {
		if _, err := ownerDB.ExecContext(ctx, "INSERT INTO favorites.owners(owner_key,closure_id) VALUES($1,$2)", account, "closure-"+account); err != nil {
			t.Fatal(err)
		}
	}
	finished := time.Now().UTC().AddDate(-1, 0, -1).Truncate(time.Microsecond)
	if err := runtime.RecordClosureCompletion(ctx, "expired", "wrong", finished); err == nil {
		t.Fatal("accepted another closure")
	}
	for range 2 {
		if err := runtime.RecordClosureCompletion(ctx, "expired", "closure-expired", finished); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.RecordClosureCompletion(ctx, "expired", "closure-expired", finished.Add(time.Hour)); err == nil {
		t.Fatal("changed completion time")
	}
	if err := runtime.RecordClosureCompletion(ctx, "current", "closure-current", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{1, 0} {
		n, err := runtime.PruneClosureMarkers(ctx, 1)
		if err != nil || n != want {
			t.Fatalf("bounded purge: %d %v", n, err)
		}
	}
	var retained int
	if err := ownerDB.QueryRowContext(ctx, "SELECT count(*) FROM favorites.owners WHERE owner_key IN ('current','incomplete')").Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("current markers: %d %v", retained, err)
	}
}
