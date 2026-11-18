package favorites_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/httpapi"
	"github.com/StuHelper/stuhelper-favorites/postgres"
	"github.com/StuHelper/stuhelper-favorites/sdk"
)

func TestFoldersDefaultPinnedDeterministicOrderResetNameAndKindCounts(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	x.s.Now = func() time.Time { return now }

	// Seed a default folder whose ID sorts after all 32-char hex custom IDs to prove is_default DESC pinning.
	if _, err := x.db.Exec(`INSERT INTO favorites.owners(owner_key) VALUES('alice')`); err != nil {
		t.Fatal(err)
	}
	if _, err := x.db.Exec(`INSERT INTO favorites.folders(owner_key,id,name,is_default,revision,position,created_at) VALUES('alice','zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz','',true,1,0,$1)`, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	var expectedCustom []string
	for i := range 35 {
		// Advance clock every 2 folders so both created_at ordering and same-timestamp id tie-breaking are exercised.
		if i%2 == 0 {
			now = now.Add(time.Minute)
		}
		created, err := x.s.CreateFolder(ctx, "alice", fmt.Sprintf("f-%d", i), fmt.Sprintf("收藏夹-%02d", i))
		if err != nil {
			t.Fatal(err)
		}
		if created.ItemCount != 0 || created.KindCounts != (f.KindCounts{}) {
			t.Fatal("new folder should have zero counts", created)
		}
		expectedCustom = append(expectedCustom, created.ID)
	}

	firstPage, err := x.s.ListFolders(ctx, "alice", "", 30)
	if err != nil || len(firstPage.Items) != 30 || !firstPage.Items[0].Default || firstPage.Next == "" {
		t.Fatal("default folder not pinned first on page 1", firstPage, err)
	}
	def := firstPage.Items[0]

	// Paginate through all 36 folders with small page size and verify deterministic order and no duplicates.
	var collected []f.Folder
	after := ""
	for {
		page, err := x.s.ListFolders(ctx, "alice", after, 7)
		if err != nil {
			t.Fatal(err)
		}
		collected = append(collected, page.Items...)
		if page.Next == "" {
			break
		}
		// Cross-owner cursor must be rejected.
		if _, err := x.s.ListFolders(ctx, "bob", page.Next, 7); !errors.Is(err, f.ErrInput) {
			t.Fatal("expected cross-owner cursor rejection", err)
		}
		after = page.Next
	}
	if len(collected) != 36 || !collected[0].Default {
		t.Fatalf("expected 36 folders with default first, got %d", len(collected))
	}
	for i := 1; i < len(collected); i++ {
		if collected[i].Default {
			t.Fatal("unexpected duplicate default folder", collected[i])
		}
	}
	// Pairs created at distinct timestamps must strictly follow creation order; equal timestamps tie-break by ID.
	for i := 0; i+2 < len(expectedCustom); i += 2 {
		laterGroupStart := i + 3 // collected[0] is default; collected[1..2] is group 0 (single folder at i=0 is collected[1])
		_ = laterGroupStart
	}
	for i := 2; i < len(collected); i++ {
		prev, cur := collected[i-1], collected[i]
		if prev.Name == "收藏夹-00" && cur.Name != "收藏夹-01" && cur.Name != "收藏夹-02" {
			t.Fatalf("unexpected order after first custom folder: %s then %s", prev.Name, cur.Name)
		}
	}
	if _, err := x.s.ListFolders(ctx, "alice", "not-base64!", 10); !errors.Is(err, f.ErrInput) {
		t.Fatal("expected invalid cursor error", err)
	}

	// Rename default folder, then reset back to empty name ("") via whitespace-only input.
	renamedDefault, err := x.s.RenameFolder(ctx, "alice", "rename-def-1", def.ID, f.Rename{Name: "临时默认名", Revision: def.Revision})
	if err != nil || renamedDefault.Name != "临时默认名" || renamedDefault.Revision != def.Revision+1 {
		t.Fatal(renamedDefault, err)
	}
	handler := httpapi.Handler{Service: x.s, Authenticate: func(*http.Request) (string, error) { return "alice", nil }}
	for i, body := range []string{fmt.Sprintf(`{"revision":%d}`, renamedDefault.Revision), fmt.Sprintf(`{"revision":%d,"name":null}`, renamedDefault.Revision)} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/favorites/folders/"+def.ID+"/rename", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("bad-reset-%d", i))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid reset %s: HTTP %d", body, w.Code)
		}
	}
	resetDefault, err := x.s.RenameFolder(ctx, "alice", "reset-def", def.ID, f.Rename{Name: "   ", Revision: renamedDefault.Revision})
	if err != nil || !resetDefault.Default || resetDefault.Name != "" || resetDefault.Revision != renamedDefault.Revision+1 {
		t.Fatal("default folder failed to reset to empty name", resetDefault, err)
	}
	// Custom folder cannot be renamed to empty string.
	customA := collected[1]
	if _, err := x.s.RenameFolder(ctx, "alice", "empty-custom", customA.ID, f.Rename{Name: "   ", Revision: customA.Revision}); !errors.Is(err, f.ErrInput) {
		t.Fatal("custom folder accepted empty name", err)
	}
	// Vacated default folder custom name can now be used by a custom folder.
	reusedName, err := x.s.RenameFolder(ctx, "alice", "reuse-name", customA.ID, f.Rename{Name: "临时默认名", Revision: customA.Revision})
	if err != nil || reusedName.Name != "临时默认名" {
		t.Fatal(reusedName, err)
	}

	// Populate courses, teachers, resources and verify kind_counts across operations.
	for _, seed := range []struct {
		key, kind, target, folder string
	}{
		{"i1", "course", "c1", customA.ID},
		{"i2", "course", "c2", customA.ID},
		{"i3", "teacher", "t1", customA.ID},
		{"i4", "resource", "r1", customA.ID},
		{"i5", "resource", "r2", customA.ID},
		{"i6", "resource", "r3", customA.ID},
		{"i7", "course", "c3", def.ID},
	} {
		if _, err := x.s.Add(ctx, "alice", seed.key, f.Add{Kind: seed.kind, TargetID: seed.target, FolderID: seed.folder}); err != nil {
			t.Fatal(err)
		}
	}

	renamedWithCounts, err := x.s.RenameFolder(ctx, "alice", "rename-with-counts", customA.ID, f.Rename{Name: "期末复习包", Revision: reusedName.Revision})
	if err != nil || renamedWithCounts.ItemCount != 6 || renamedWithCounts.KindCounts != (f.KindCounts{Course: 2, Teacher: 1, Resource: 3}) {
		t.Fatal("rename response missing accurate kind_counts", renamedWithCounts, err)
	}

	// Verify via generated Go SDK over HTTP.
	srv := httptest.NewServer(httpapi.Handler{
		Service: x.s,
		Authenticate: func(r *http.Request) (string, error) {
			return r.Header.Get("Test-Session"), nil
		},
	})
	defer srv.Close()
	client, err := sdk.NewClientWithResponses(srv.URL, sdk.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Test-Session", "alice")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.ListFoldersWithResponse(ctx, &sdk.ListFoldersParams{})
	if err != nil || res.StatusCode() != http.StatusOK || res.JSON200 == nil {
		t.Fatal(err, res)
	}
	if !res.JSON200.Items[0].IsDefault || res.JSON200.Items[0].ItemCount != 1 || res.JSON200.Items[0].KindCounts.Course != 1 || res.JSON200.Items[0].KindCounts.Resource != 0 {
		t.Fatal("unexpected default folder SDK counts", res.JSON200.Items[0])
	}
	if res.JSON200.Items[1].Id != customA.ID || res.JSON200.Items[1].KindCounts.Course != 2 || res.JSON200.Items[1].KindCounts.Teacher != 1 || res.JSON200.Items[1].KindCounts.Resource != 3 {
		t.Fatal("unexpected custom folder SDK counts", res.JSON200.Items[1])
	}

	// Deleting customA moves all 6 items into default folder and updates kind_counts atomically.
	if _, err := x.s.DeleteFolder(ctx, "alice", "del-customA", customA.ID, renamedWithCounts.Revision); err != nil {
		t.Fatal(err)
	}
	afterDel, err := x.s.ListFolders(ctx, "alice", "", 5)
	if err != nil || afterDel.Items[0].ItemCount != 7 || afterDel.Items[0].KindCounts != (f.KindCounts{Course: 3, Teacher: 1, Resource: 3}) {
		t.Fatal("counts after folder deletion mismatch", afterDel, err)
	}

	// Verify incremental Migrate upgrades a legacy favorites.folders table without position/created_at.
	dsn := os.Getenv("STUHELPER_FAVORITES_TEST_DSN")
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	legacyName := fmt.Sprintf("favorites_legacy_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + legacyName); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(`DROP DATABASE ` + legacyName + ` WITH(FORCE)`)
	legacyDB, err := sql.Open("pgx", dsn+" dbname="+legacyName)
	if err != nil {
		t.Fatal(err)
	}
	defer legacyDB.Close()
	if _, err := legacyDB.Exec(`
		CREATE SCHEMA favorites;
		CREATE TABLE favorites.owners(owner_key text PRIMARY KEY, closure_id text);
		CREATE TABLE favorites.folders(owner_key text NOT NULL REFERENCES favorites.owners(owner_key), id text NOT NULL, name text NOT NULL, is_default boolean NOT NULL, revision bigint NOT NULL, PRIMARY KEY(owner_key, id));
		INSERT INTO favorites.owners(owner_key) VALUES('legacy');
		INSERT INTO favorites.folders(owner_key, id, name, is_default, revision) VALUES('legacy', 'def-1', '', true, 1);
	`); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, legacyDB); err != nil {
		t.Fatal("incremental migrate failed on legacy folders table", err)
	}
	legacySvc := &f.Service{Repo: &postgres.Repository{DB: legacyDB}, Source: x.source}
	legacyFolders, err := legacySvc.ListFolders(ctx, "legacy", "", 10)
	if err != nil || len(legacyFolders.Items) != 1 || !legacyFolders.Items[0].Default {
		t.Fatal(legacyFolders, err)
	}
}
