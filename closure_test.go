package favorites_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	f "github.com/StuHelper/stuhelper-favorites"
)

func TestClosureDeletesCollectionsAtomicallyAndPreventsResurrection(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	for _, owner := range []string{"alice", "bob"} {
		if _, e := x.s.CreateFolder(ctx, owner, "folder", "Study"); e != nil {
			t.Fatal(e)
		}
		if _, e := x.s.Add(ctx, owner, "item", f.Add{Kind: "course", TargetID: "course"}); e != nil {
			t.Fatal(e)
		}
	}
	id := "11111111-1111-4111-8111-111111111111"
	if _, e := x.db.Exec(`CREATE FUNCTION favorites.reject_close() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic'; END $$; CREATE TRIGGER reject_close BEFORE INSERT ON favorites.events FOR EACH ROW WHEN(NEW.kind='owner.close') EXECUTE FUNCTION favorites.reject_close()`); e != nil {
		t.Fatal(e)
	}
	if e := x.s.CloseOwner(ctx, "alice", id); !errors.Is(e, f.ErrUnavailable) {
		t.Fatal("failed transaction accepted", e)
	}
	page, e := x.s.List(ctx, "alice", f.Filter{Limit: 10})
	if e != nil || len(page.Items) != 1 {
		t.Fatal("partial deletion", page, e)
	}
	if _, e = x.db.Exec(`DROP TRIGGER reject_close ON favorites.events`); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if e := x.s.CloseOwner(ctx, "alice", id); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	for _, table := range []string{"items", "folders", "requests"} {
		var n int
		if e = x.db.QueryRow(`SELECT count(*) FROM favorites.` + table + ` WHERE owner_key='alice'`).Scan(&n); e != nil || n != 0 {
			t.Fatal("personal data retained", table, n, e)
		}
	}
	if _, e = x.s.Add(ctx, "alice", "item", f.Add{Kind: "course", TargetID: "course"}); !errors.Is(e, f.ErrForbidden) {
		t.Fatal("replay restored closed collection", e)
	}
	if _, e = x.s.ListFolders(ctx, "alice", "", 10); !errors.Is(e, f.ErrForbidden) {
		t.Fatal("read restored default folder", e)
	}
	page, e = x.s.List(ctx, "bob", f.Filter{Limit: 10})
	if e != nil || len(page.Items) != 1 {
		t.Fatal("unrelated owner changed", page, e)
	}
	var n int
	if e = x.db.QueryRow(`SELECT count(*) FROM favorites.events WHERE owner_key='alice' AND kind='owner.close'`).Scan(&n); e != nil || n != 1 {
		t.Fatal("duplicate closure", n, e)
	}
	if e = x.s.CloseOwner(ctx, "alice", "22222222-2222-4222-8222-222222222222"); !errors.Is(e, f.ErrConflict) {
		t.Fatal("closure identifier mismatch accepted", e)
	}
}
