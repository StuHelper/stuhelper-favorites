package favorites_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/httpapi"
	"github.com/StuHelper/stuhelper-favorites/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type source struct {
	state atomic.Int32
	calls atomic.Int32
}

func (s *source) Resolve(ctx context.Context, owner, kind, id string) (string, error) {
	s.calls.Add(1)
	if _, ok := ctx.Deadline(); !ok {
		return "", errors.New("no timeout")
	}
	switch s.state.Load() {
	case 1:
		return "", f.ErrForbidden
	case 2:
		return "", f.ErrUnavailable
	case 3:
		return "", context.DeadlineExceeded
	}
	return "Title " + id, nil
}

type emptyLegacy struct{}

func (emptyLegacy) Page(context.Context, string, string, int) (f.LegacyPage, error) {
	return f.LegacyPage{Items: []f.LegacyItem{}}, nil
}

type fixture struct {
	s      *f.Service
	source *source
	db     *sql.DB
	repo   *postgres.Repository
}

func setup(t *testing.T) fixture {
	t.Helper()
	dsn := os.Getenv("STUHELPER_FAVORITES_TEST_DSN")
	if dsn == "" {
		t.Fatal("isolated PostgreSQL required")
	}
	admin, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	name := fmt.Sprintf("favorites_%d", time.Now().UnixNano())
	if _, e = admin.Exec(`CREATE DATABASE ` + name); e != nil {
		t.Fatal(e)
	}
	owner, e := sql.Open("pgx", dsn+" dbname="+name)
	if e != nil {
		t.Fatal(e)
	}
	role := name + "_runtime"
	t.Cleanup(func() {
		owner.Close()
		admin.Exec(`DROP DATABASE ` + name + ` WITH(FORCE)`)
		admin.Exec(`DROP ROLE ` + role)
		admin.Close()
	})
	for range 2 {
		if e = postgres.Migrate(context.Background(), owner); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = owner.Exec(`CREATE ROLE ` + role + ` LOGIN;REVOKE ALL ON SCHEMA public FROM PUBLIC;GRANT USAGE ON SCHEMA favorites TO ` + role + `;GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA favorites TO ` + role + `;GRANT USAGE ON ALL SEQUENCES IN SCHEMA favorites TO ` + role + `;REVOKE UPDATE ON favorites.requests FROM ` + role + `;REVOKE UPDATE,DELETE ON favorites.events FROM ` + role + `;CREATE SCHEMA foreign_data;CREATE TABLE foreign_data.secret(value text);`); e != nil {
		t.Fatal(e)
	}
	runtime, e := sql.Open("pgx", dsn+" dbname="+name+" user="+role)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { runtime.Close() })
	repo := &postgres.Repository{DB: runtime}
	if e = repo.CheckRuntime(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = (&postgres.Repository{DB: owner}).CheckRuntime(context.Background()); e == nil {
		t.Fatal("migration identity accepted")
	}
	if _, e = runtime.Exec(`SELECT * FROM foreign_data.secret`); e == nil {
		t.Fatal("foreign schema readable")
	}
	src := &source{}
	s := &f.Service{Repo: repo, Source: src, Legacy: emptyLegacy{}, Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }}
	return fixture{s, src, owner, repo}
}
func TestFavoritesOwnershipFoldersAdmissionAndConcurrentReplay(t *testing.T) {
	x := setup(t)
	s := x.s
	ctx := context.Background()
	folders, e := s.ListFolders(ctx, "alice", "", 30)
	if e != nil || len(folders.Items) != 1 || !folders.Items[0].Default || folders.Items[0].Name != "" {
		t.Fatal(folders, e)
	}
	def := folders.Items[0]
	if _, e = s.DeleteFolder(ctx, "alice", "delete-default", def.ID, 1); !errors.Is(e, f.ErrForbidden) {
		t.Fatal(e)
	}
	renamed, e := s.RenameFolder(ctx, "alice", "rename-default", def.ID, f.Rename{Name: strings.Repeat("𠮷", 20), Revision: 1})
	if e != nil || renamed.Revision != 2 {
		t.Fatal(renamed, e)
	}
	if _, e = s.CreateFolder(ctx, "alice", "long", strings.Repeat("𠮷", 21)); !errors.Is(e, f.ErrInput) {
		t.Fatal(e)
	}
	folder, e := s.CreateFolder(ctx, "alice", "new-folder", "  学习  ")
	if e != nil || folder.Name != "学习" {
		t.Fatal(folder, e)
	}
	if _, e = s.CreateFolder(ctx, "alice", "duplicate", " 学习 "); !errors.Is(e, f.ErrConflict) {
		t.Fatal("duplicate folder name", e)
	}
	if _, e = s.RenameFolder(ctx, "alice", "default-duplicate", def.ID, f.Rename{Name: "学习", Revision: 2}); !errors.Is(e, f.ErrConflict) {
		t.Fatal("default renamed to duplicate", e)
	}
	if _, e = s.CreateFolder(ctx, "bob", "same-name-other-owner", "学习"); e != nil {
		t.Fatal(e)
	}
	collision := make(chan error, 2)
	var collisionWG sync.WaitGroup
	for _, key := range []string{"collision-a", "collision-b"} {
		collisionWG.Go(func() { _, e := s.CreateFolder(ctx, "alice", key, "Concurrent"); collision <- e })
	}
	collisionWG.Wait()
	close(collision)
	success, conflict := 0, 0
	for e := range collision {
		if e == nil {
			success++
		} else if errors.Is(e, f.ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
	if _, e = s.RenameFolder(ctx, "bob", "attack", folder.ID, f.Rename{Name: "hijack", Revision: 1}); !errors.Is(e, f.ErrNotFound) {
		t.Fatal(e)
	}
	results := make(chan f.Item, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			v, e := s.Add(ctx, "alice", "same", f.Add{Kind: "course", TargetID: "course", FolderID: folder.ID})
			results <- v
			errs <- e
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	var course f.Item
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	for v := range results {
		if course.ID != "" && course.ID != v.ID {
			t.Fatal("duplicate ID")
		}
		course = v
	}
	var count int
	if e = x.db.QueryRow(`SELECT count(*) FROM favorites.items WHERE owner_key='alice'`).Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	if _, e = s.Add(ctx, "alice", "same", f.Add{Kind: "teacher", TargetID: "teacher"}); !errors.Is(e, f.ErrConflict) {
		t.Fatal(e)
	}
	for _, kind := range []string{"teacher", "resource"} {
		if _, e = s.Add(ctx, "alice", kind, f.Add{Kind: kind, TargetID: kind}); e != nil {
			t.Fatal(e)
		}
	}
	page, e := s.List(ctx, "alice", f.Filter{Limit: 1})
	if e != nil || len(page.Items) != 1 || page.Next == "" {
		t.Fatal(page, e)
	}
	seen := map[string]bool{page.Items[0].ID: true}
	for page.Next != "" {
		page, e = s.List(ctx, "alice", f.Filter{Limit: 1, After: page.Next})
		if e != nil || len(page.Items) != 1 || seen[page.Items[0].ID] {
			t.Fatal(page, e)
		}
		seen[page.Items[0].ID] = true
	}
	if len(seen) != 3 {
		t.Fatal(seen)
	}
	sorted, e := s.List(ctx, "alice", f.Filter{Limit: 10, Sort: "name", Query: "teacher"})
	if e != nil || len(sorted.Items) != 1 || sorted.Items[0].Kind != "teacher" {
		t.Fatal(sorted, e)
	}
	x.source.state.Store(1)
	calls := x.source.calls.Load()
	if _, e = s.Add(ctx, "alice", "replay-existing", f.Add{Kind: "course", TargetID: "course"}); e != nil {
		t.Fatal(e)
	}
	if x.source.calls.Load() != calls {
		t.Fatal("own existing record queried admission")
	}
	if _, e = s.Add(ctx, "alice", "denied", f.Add{Kind: "course", TargetID: "new"}); !errors.Is(e, f.ErrForbidden) {
		t.Fatal(e)
	}
	x.source.state.Store(2)
	if _, e = s.Add(ctx, "alice", "unavailable", f.Add{Kind: "resource", TargetID: "new"}); !errors.Is(e, f.ErrUnavailable) {
		t.Fatal(e)
	}
	moved, e := s.Move(ctx, "alice", "move", course.ID, f.Move{FolderID: def.ID, Revision: 1})
	if e != nil || moved.Revision != 2 {
		t.Fatal(moved, e)
	}
	if _, e = s.Move(ctx, "bob", "move", course.ID, f.Move{FolderID: def.ID, Revision: 2}); !errors.Is(e, f.ErrNotFound) {
		t.Fatal(e)
	}
	empty, e := s.List(ctx, "alice", f.Filter{FolderID: folder.ID, Limit: 20})
	if e != nil || len(empty.Items) != 0 {
		t.Fatal(empty, e)
	}
	moved, e = s.Move(ctx, "alice", "move-back", course.ID, f.Move{FolderID: folder.ID, Revision: 2})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DeleteFolder(ctx, "alice", "delete", folder.ID, 1); e != nil {
		t.Fatal(e)
	}
	own, e := s.List(ctx, "alice", f.Filter{Kind: "course", Limit: 20})
	if e != nil || len(own.Items) != 1 || own.Items[0].FolderID != def.ID || own.Items[0].Revision != moved.Revision+1 {
		t.Fatal(own, e)
	}
	if _, e = s.Remove(ctx, "alice", "stale-remove", course.ID, moved.Revision); !errors.Is(e, f.ErrConflict) {
		t.Fatal(e)
	}
	removed, e := s.Remove(ctx, "alice", "remove", course.ID, own.Items[0].Revision)
	if e != nil || !removed.Removed {
		t.Fatal(removed, e)
	}
	if _, e = s.Remove(ctx, "alice", "remove", course.ID, own.Items[0].Revision); e != nil {
		t.Fatal(e)
	}
	bob, e := s.List(ctx, "bob", f.Filter{Limit: 20})
	if e != nil || len(bob.Items) != 0 {
		t.Fatal(bob, e)
	}
	h := httpapi.Handler{Service: s, Authenticate: func(r *http.Request) (string, error) { return r.Header.Get("Test-Session"), nil }}
	request := func(method, path, body, owner string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/v1/favorites/"+path, strings.NewReader(body))
		r.Header.Set("Test-Session", owner)
		r.Header.Set("Idempotency-Key", "http")
		r.Header.Set("X-Owner", "alice")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "items", "", ""); w.Code != 401 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("GET", "items?owner=alice", "", "bob"); w.Code != 200 || strings.Contains(w.Body.String(), "teacher") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", "items", `{"kind":"resource","target_id":"new","title":"forged"}`, "alice"); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", "items", `{"kind":"resource","target_id":"new"}`, "alice"); w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("GET", "items?limit=101", "", "alice"); w.Code != 400 {
		t.Fatal(w.Code)
	}
	// Local business writes, idempotency and the audit event must roll back together.
	if _, e = x.db.Exec(`CREATE FUNCTION favorites.reject_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic'; END $$;CREATE TRIGGER reject_event BEFORE INSERT ON favorites.events FOR EACH ROW WHEN(NEW.operation='rollback') EXECUTE FUNCTION favorites.reject_event()`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateFolder(ctx, "alice", "rollback", "Must rollback"); !errors.Is(e, f.ErrUnavailable) {
		t.Fatal(e)
	}
	if e = x.db.QueryRow(`SELECT count(*) FROM favorites.folders WHERE name='Must rollback'`).Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
	if e = x.db.QueryRow(`SELECT count(*) FROM favorites.requests WHERE key='rollback'`).Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
}

type legacy struct {
	fail  atomic.Bool
	calls atomic.Int32
}

func (l *legacy) Page(ctx context.Context, owner, after string, limit int) (f.LegacyPage, error) {
	l.calls.Add(1)
	if after != "" && l.fail.Load() {
		return f.LegacyPage{}, f.ErrUnavailable
	}
	p := f.LegacyPage{Items: []f.LegacyItem{}}
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("old-%03d", i)
		if id <= after {
			continue
		}
		p.Items = append(p.Items, f.LegacyItem{TargetID: id, Title: "Old " + id})
		if len(p.Items) == limit {
			p.Next = id
			break
		}
	}
	return p, nil
}
func TestLegacyImportResumesWithoutDuplicatesOrResurrection(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	old := &legacy{}
	old.fail.Store(true)
	x.s.Legacy = old
	if _, e := x.s.List(ctx, "alice", f.Filter{Limit: 100}); !errors.Is(e, f.ErrUnavailable) {
		t.Fatal(e)
	}
	var count int
	if e := x.db.QueryRow(`SELECT count(*) FROM favorites.items WHERE owner_key='alice'`).Scan(&count); e != nil || count != 100 {
		t.Fatal(count, e)
	}
	old.fail.Store(false)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { _, e := x.s.List(ctx, "alice", f.Filter{Limit: 100}); errs <- e })
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if e := x.db.QueryRow(`SELECT count(*) FROM favorites.items WHERE owner_key='alice'`).Scan(&count); e != nil || count != 105 {
		t.Fatal(count, e)
	}
	calls := old.calls.Load()
	x.source.state.Store(2)
	old.fail.Store(true)
	if e := x.s.SetCourseBookmark(ctx, "alice", "old-000", "remove-old", false); e != nil {
		t.Fatal(e)
	}
	if _, e := x.s.List(ctx, "alice", f.Filter{Limit: 100}); e != nil {
		t.Fatal(e)
	}
	if old.calls.Load() != calls {
		t.Fatal("completed import reread old store")
	}
	if e := x.db.QueryRow(`SELECT count(*) FROM favorites.items WHERE owner_key='alice' AND target_id='old-000'`).Scan(&count); e != nil || count != 0 {
		t.Fatal("resurrected", count, e)
	}
	x.s.Legacy = nil
	if _, e := x.s.List(ctx, "new-account", f.Filter{Limit: 30}); !errors.Is(e, f.ErrUnavailable) {
		t.Fatal("unknown legacy store treated as empty", e)
	}
}
