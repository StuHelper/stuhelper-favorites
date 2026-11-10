package favorites_test

import (
	"context"
	"fmt"
	f "github.com/StuHelper/stuhelper-favorites"
	"sync"
	"testing"
	"time"
)

func TestActivityNetFavoritesCrossDayCancellationAndConcurrentIdempotency(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	day := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	now := day.Add(time.Hour)
	x.s.Now = func() time.Time { return now }
	item, e := x.s.Add(ctx, "owner", "add", f.Add{Kind: "course", TargetID: "course"})
	if e != nil {
		t.Fatal(e)
	}
	assert := func(day time.Time, n int64) f.Activity {
		t.Helper()
		d, e := x.repo.ReadActivity(ctx, f.ActivityID("course", "course", day))
		if e != nil || d.Metrics["favorites"] != n {
			t.Fatal(d, e)
		}
		return d
	}
	before := assert(day, 1)
	if _, e = x.s.Add(ctx, "owner", "duplicate", f.Add{Kind: "course", TargetID: "course"}); e != nil {
		t.Fatal(e)
	}
	if d := assert(day, 1); d.Revision != before.Revision {
		t.Fatal("duplicate added a contribution", d)
	}
	folder, e := x.s.CreateFolder(ctx, "owner", "folder", "Folder")
	if e != nil {
		t.Fatal(e)
	}
	item, e = x.s.Move(ctx, "owner", "move", item.ID, f.Move{FolderID: folder.ID, Revision: item.Revision})
	if e != nil {
		t.Fatal(e)
	}
	if d := assert(day, 1); d.Revision != before.Revision {
		t.Fatal("move counted", d)
	}
	now = now.Add(24 * time.Hour)
	if _, e = x.s.Remove(ctx, "owner", "cancel-next-day", item.ID, item.Revision); e != nil {
		t.Fatal(e)
	}
	assert(day, 1)
	zero := assert(day.AddDate(0, 0, 1), 0)
	if zero.Visible {
		t.Fatal(zero)
	}
	// Retaining a signed net makes cancellation-before-add order independent.
	if _, e = x.s.Add(ctx, "owner", "readd", f.Add{Kind: "course", TargetID: "course"}); e != nil {
		t.Fatal(e)
	}
	next := assert(day.AddDate(0, 0, 1), 0)
	if next.Revision <= zero.Revision {
		t.Fatal(next)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, e := x.s.Add(ctx, fmt.Sprintf("other-%d", i), "add", f.Add{Kind: "course", TargetID: "course"})
			errs <- e
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	assert(day.AddDate(0, 0, 1), 8)
	if _, ok, e := x.s.CurrentActivity(ctx, "owner", f.ActivityID("course", "course", day)); e != nil || !ok {
		t.Fatal(ok, e)
	}
	x.source.state.Store(1)
	if _, ok, e := x.s.CurrentActivity(ctx, "owner", f.ActivityID("course", "course", day)); e != nil || ok {
		t.Fatal(ok, e)
	}
	x.source.state.Store(2)
	if _, _, e := x.s.CurrentActivity(ctx, "owner", f.ActivityID("course", "course", day)); e == nil {
		t.Fatal("source outage hidden")
	}
}

func TestActivityFavoriteWriteAndClosureRollbackWithOutbox(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	if _, e := x.db.Exec(`ALTER TABLE favorites.outbox ADD CONSTRAINT fail_activity CHECK(false) NOT VALID`); e != nil {
		t.Fatal(e)
	}
	if _, e := x.s.Add(ctx, "owner", "add", f.Add{Kind: "teacher", TargetID: "teacher"}); e == nil {
		t.Fatal("failed activity write accepted")
	}
	var n int
	if e := x.db.QueryRow(`SELECT count(*) FROM favorites.items`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if _, e := x.db.Exec(`ALTER TABLE favorites.outbox DROP CONSTRAINT fail_activity`); e != nil {
		t.Fatal(e)
	}
	if _, e := x.s.Add(ctx, "owner", "add", f.Add{Kind: "teacher", TargetID: "teacher"}); e != nil {
		t.Fatal(e)
	}
	if _, e := x.db.Exec(`ALTER TABLE favorites.outbox ADD CONSTRAINT fail_activity CHECK(false) NOT VALID`); e != nil {
		t.Fatal(e)
	}
	if e := x.repo.CloseOwner(ctx, "owner", "close"); e == nil {
		t.Fatal("closure ignored activity failure")
	}
	if e := x.db.QueryRow(`SELECT count(*) FROM favorites.items`).Scan(&n); e != nil || n != 1 {
		t.Fatal("failed closure deleted item", n, e)
	}
	if _, e := x.db.Exec(`ALTER TABLE favorites.outbox DROP CONSTRAINT fail_activity`); e != nil {
		t.Fatal(e)
	}
	if e := x.repo.CloseOwner(ctx, "owner", "close"); e != nil {
		t.Fatal(e)
	}
	if e := x.repo.CloseOwner(ctx, "owner", "close"); e != nil {
		t.Fatal(e)
	}
	if e := x.db.QueryRow(`SELECT count(*) FROM favorites.items`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}

func TestActivityFavoritesUseCanonicalTargetWithoutChangingOwnReceipt(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	item, e := x.s.Add(ctx, "owner", "alias", f.Add{Kind: "course", TargetID: "old-alias"})
	if e != nil {
		t.Fatal(e)
	}
	if item.TargetID != "old-alias" {
		t.Fatal("own receipt target changed", item)
	}
	d, e := x.repo.ReadActivity(ctx, f.ActivityID("course", "course", x.s.Now()))
	if e != nil || d.Target.ID != "course" || d.Metrics["favorites"] != 1 {
		t.Fatal(d, e)
	}
	if _, e = x.repo.ReadActivity(ctx, f.ActivityID("course", "old-alias", x.s.Now())); e == nil {
		t.Fatal("alias got separate activity")
	}
	if _, e = x.s.Remove(ctx, "owner", "remove", item.ID, item.Revision); e != nil {
		t.Fatal(e)
	}
	d, e = x.repo.ReadActivity(ctx, d.ID)
	if e != nil || d.Metrics["favorites"] != 0 {
		t.Fatal(d, e)
	}
}
