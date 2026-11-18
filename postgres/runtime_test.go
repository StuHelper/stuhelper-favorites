package postgres

import (
	"context"
	"errors"
	f "github.com/StuHelper/stuhelper-favorites"
	"testing"
)

func TestRuntimePrivilegeIsolationAndFullTableReadiness(t *testing.T) {
	repo, cleaner, owner := activityDatabase(t)
	ctx := context.Background()
	var role string
	if err := repo.DB.QueryRow("SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := owner.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"SELECT ON favorites.items", "INSERT ON favorites.outbox", "USAGE ON SEQUENCE favorites.events_id_seq", "USAGE ON SCHEMA favorites"} {
		exec("REVOKE " + p + " FROM " + role)
		if !errors.Is(repo.Ready(ctx), f.ErrUnavailable) || !errors.Is(repo.CheckRuntime(ctx), f.ErrUnavailable) {
			t.Fatalf("missing required %s", p)
		}
		exec("GRANT " + p + " TO " + role)
	}
	for _, p := range []string{"UPDATE ON favorites.activity_history", "DELETE ON favorites.activity_history", "UPDATE ON favorites.activity_retention", "DELETE ON favorites.outbox", "DELETE ON favorites.activity_buckets", "TRUNCATE ON favorites.events"} {
		exec("GRANT " + p + " TO " + role)
		if !errors.Is(repo.CheckRuntime(ctx), f.ErrUnavailable) {
			t.Fatalf("accepted excessive %s", p)
		}
		exec("REVOKE " + p + " FROM " + role)
	}
	if err := repo.CheckRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(cleaner.CheckRuntime(ctx), f.ErrUnavailable) {
		t.Fatal("cleaner passed business runtime check")
	}
	exec("DROP TABLE favorites.outbox")
	if !errors.Is(repo.Ready(ctx), f.ErrUnavailable) {
		t.Fatal("missing outbox")
	}
}
