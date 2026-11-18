package favorites_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/postgres"
)

type blockingSource struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingSource) Resolve(ctx context.Context, owner, kind, id string) (f.ResolvedTarget, error) {
	b.calls.Add(1)
	if kind == "resource" {
		b.entered <- struct{}{}
		select {
		case <-b.release:
		case <-ctx.Done():
			return f.ResolvedTarget{}, ctx.Err()
		}
	}
	return f.ResolvedTarget{ID: id, Kind: kind, Title: "Title " + id}, nil
}

func TestAddReleasesDatabaseConnectionsDuringSlowResolveAndReadQueriesAvoidWriteLocks(t *testing.T) {
	x := setup(t)
	ctx := context.Background()

	// Verify A-07 part 1: read-only List for a brand-new user must not write owners or folders.
	items, err := x.s.List(ctx, "readonly-viewer", f.Filter{Kind: "course", TargetIDs: []string{"c1", "c2"}, Limit: 20})
	if err != nil || len(items.Items) != 0 {
		t.Fatalf("unexpected list result: %v, %v", items, err)
	}
	for _, table := range []string{"owners", "folders"} {
		var count int
		if err := x.db.QueryRow(`SELECT count(*) FROM favorites.` + table + ` WHERE owner_key='readonly-viewer'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("GET List mutated %s: count=%d err=%v", table, count, err)
		}
	}
	if _, err := x.s.List(ctx, "readonly-viewer", f.Filter{FolderID: "missing-folder", Limit: 20}); !errors.Is(err, f.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing folder, got %v", err)
	}

	// Verify A-07 part 2: List and ListFolders (once initialized) do not block on per-owner advisory write lock.
	initFolders, err := x.s.ListFolders(ctx, "locked-reader", "", 10)
	if err != nil || len(initFolders.Items) != 1 || !initFolders.Items[0].Default {
		t.Fatalf("lazy default folder init failed: %v, %v", initFolders, err)
	}
	if _, err := x.s.Add(ctx, "locked-reader", "seed-course", f.Add{Kind: "course", TargetID: "c-seed"}); err != nil {
		t.Fatal(err)
	}
	lockTx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback()
	if _, err := lockTx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "favorites:locked-reader"); err != nil {
		t.Fatal(err)
	}
	shortCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	readItems, err := x.s.List(shortCtx, "locked-reader", f.Filter{Limit: 10})
	if err != nil || len(readItems.Items) != 1 {
		t.Fatalf("List blocked on advisory write lock: %v, %v", readItems, err)
	}
	readFolders, err := x.s.ListFolders(shortCtx, "locked-reader", "", 10)
	if err != nil || len(readFolders.Items) != 1 || readFolders.Items[0].ItemCount != 1 {
		t.Fatalf("ListFolders blocked on advisory write lock: %v, %v", readFolders, err)
	}
	_ = lockTx.Rollback()

	// Verify A-01: with a minimum 2-connection pool, two slow resource Resolve calls must not starve other DB operations.
	var dbName, roleName string
	if err := x.repo.DB.QueryRowContext(ctx, `SELECT current_database(), current_user`).Scan(&dbName, &roleName); err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("STUHELPER_FAVORITES_TEST_DSN") + " dbname=" + dbName + " user=" + roleName
	pooledDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pooledDB.Close()
	pooledDB.SetMaxOpenConns(2)
	pooledDB.SetMaxIdleConns(2)

	slow := &blockingSource{entered: make(chan struct{}, 4), release: make(chan struct{})}
	svc := &f.Service{
		Repo:   &postgres.Repository{DB: pooledDB},
		Source: slow,
		Now:    x.s.Now,
	}

	var wg sync.WaitGroup
	addErrs := make(chan error, 2)
	for _, owner := range []string{"slow-a", "slow-b"} {
		wg.Go(func() {
			_, e := svc.Add(ctx, owner, "add-res", f.Add{Kind: "resource", TargetID: "res-1"})
			addErrs <- e
		})
	}
	for range 2 {
		select {
		case <-slow.entered:
		case <-time.After(time.Second):
			t.Fatal("slow Add did not enter Source.Resolve")
		}
	}

	probeCtx, probeCancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer probeCancel()
	if _, err := svc.Add(probeCtx, "fast-user", "fast-course", f.Add{Kind: "course", TargetID: "fast-c1"}); err != nil {
		t.Fatalf("other user starved while optional resource Resolve was slow: %v", err)
	}
	if _, err := svc.List(probeCtx, "slow-a", f.Filter{Limit: 10}); err != nil {
		t.Fatalf("same user List blocked during slow Resolve: %v", err)
	}

	close(slow.release)
	wg.Wait()
	close(addErrs)
	for e := range addErrs {
		if e != nil {
			t.Fatalf("slow Add failed after release: %v", e)
		}
	}

	// Precheck guarantees: replay, existing item, missing folder, and closed owner must not invoke Source.Resolve.
	beforeCalls := slow.calls.Load()
	if _, err := svc.Add(ctx, "fast-user", "fast-course", f.Add{Kind: "course", TargetID: "fast-c1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(ctx, "fast-user", "different-key-same-target", f.Add{Kind: "course", TargetID: "fast-c1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(ctx, "fast-user", "missing-folder-key", f.Add{Kind: "course", TargetID: "new-c2", FolderID: "non-existent"}); !errors.Is(err, f.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing folder, got %v", err)
	}
	if err := svc.CloseOwner(ctx, "closed-user", "33333333-3333-4333-8333-333333333333"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(ctx, "closed-user", "closed-add", f.Add{Kind: "course", TargetID: "new-c3"}); !errors.Is(err, f.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for closed owner, got %v", err)
	}
	if slow.calls.Load() != beforeCalls {
		t.Fatalf("Source.Resolve should not be called on precheck hits/rejections: before=%d after=%d", beforeCalls, slow.calls.Load())
	}

	// Failed Source.Resolve (including sub-timeout) must return ErrUnavailable and persist zero owner/folder/item rows.
	x.source.state.Store(3)
	if _, err := x.s.Add(ctx, "failed-resolve-owner", "timeout-key", f.Add{Kind: "resource", TargetID: "res-timeout"}); !errors.Is(err, f.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable on Source.Resolve timeout, got %v", err)
	}
	for _, table := range []string{"owners", "folders", "items"} {
		var count int
		if err := x.db.QueryRow(`SELECT count(*) FROM favorites.` + table + ` WHERE owner_key='failed-resolve-owner'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed Add persisted row in %s: count=%d err=%v", table, count, err)
		}
	}
}
