package favorites_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/httpapi"
	"github.com/StuHelper/stuhelper-favorites/postgres"
	"github.com/StuHelper/stuhelper-favorites/sdk"
)

func TestHTTPContractEncodingInputBoundariesAndSDKCompatibility(t *testing.T) {
	x := setup(t)
	ctx := context.Background()

	handler := httpapi.Handler{
		Service: x.s,
		Authenticate: func(r *http.Request) (string, error) {
			return r.Header.Get("Test-Session"), nil
		},
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := sdk.NewClientWithResponses(server.URL, sdk.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
		req.Header.Set("Test-Session", "alice")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	// A-04 Part 1: SDK encodes commas inside target_ids items as %2C and keeps literal comma as array delimiter.
	addComma, err := client.AddFavoriteWithResponse(ctx, &sdk.AddFavoriteParams{IdempotencyKey: "sdk-add-comma"}, sdk.Add{
		Kind:     sdk.AddKindCourse,
		TargetId: "course,1",
	})
	if err != nil || addComma.StatusCode() != 200 || addComma.JSON200 == nil {
		t.Fatalf("SDK AddFavorite failed: status=%v err=%v", addComma.StatusCode(), err)
	}
	kindCourse := sdk.ListFavoritesParamsKindCourse
	targets := []string{"course,1", "missing,2"}
	listed, err := client.ListFavoritesWithResponse(ctx, &sdk.ListFavoritesParams{
		Kind:      &kindCourse,
		TargetIds: &targets,
	})
	if err != nil || listed.StatusCode() != 200 || listed.JSON200 == nil || len(listed.JSON200.Items) != 1 || listed.JSON200.Items[0].TargetId != "course,1" {
		t.Fatalf("SDK ListFavorites with comma target_id failed: status=%d body=%s err=%v", listed.StatusCode(), string(listed.Body), err)
	}

	largeIDs := make([]string, 100)
	for i := range largeIDs {
		largeIDs[i] = strings.Repeat("学", 197) + fmt.Sprintf("%03d", i)
	}
	large, err := client.ListFavoritesWithResponse(ctx, &sdk.ListFavoritesParams{Kind: &kindCourse, TargetIds: &largeIDs})
	if err != nil || large.JSON200 == nil {
		t.Fatalf("percent-encoded 100 Unicode IDs: %v %+v", err, large)
	}

	// 100 target_ids where one contains a comma must still be accepted as 100 items, not split into 101.
	hundred := make([]string, 0, 100)
	hundred = append(hundred, "course,1")
	for i := 1; i < 100; i++ {
		hundred = append(hundred, fmt.Sprintf("absent-%d", i))
	}
	listed100, err := client.ListFavoritesWithResponse(ctx, &sdk.ListFavoritesParams{
		Kind:      &kindCourse,
		TargetIds: &hundred,
	})
	if err != nil || listed100.StatusCode() != 200 || listed100.JSON200 == nil || len(listed100.JSON200.Items) != 1 {
		t.Fatalf("expected 200 with 100 SDK target_ids containing comma, got status=%d body=%s err=%v", listed100.StatusCode(), string(listed100.Body), err)
	}

	// Encoded %2F in path parameter {id} must be unescaped as part of {id} (yielding 404 owned record not found, or handled by service, rather than wrong route split).
	if _, err := x.db.Exec(`INSERT INTO favorites.items(owner_key,id,kind,target_id,canonical_target_id,title,folder_id,revision,created_at) VALUES('alice','item/slash','resource','res-slash','res-slash','Slash Title',$1,1,now())`, addComma.JSON200.FolderId); err != nil {
		t.Fatal(err)
	}
	rmSlash, err := client.RemoveFavoriteWithResponse(ctx, "item/slash", &sdk.RemoveFavoriteParams{IdempotencyKey: "sdk-rm-slash"}, sdk.Revision{Revision: 1})
	if err != nil || rmSlash.StatusCode() != 200 || rmSlash.JSON200 == nil || !rmSlash.JSON200.Removed {
		t.Fatalf("RemoveFavorite with slash in {id} failed: status=%d body=%s err=%v", rmSlash.StatusCode(), string(rmSlash.Body), err)
	}

	// A-04 Part 2: Add and CourseReferences must reject leading/trailing whitespace in target_id.
	for _, badTarget := range []string{" course", "course ", "\tcourse"} {
		if _, err := x.s.Add(ctx, "alice", "ws-add", f.Add{Kind: "course", TargetID: badTarget}); !errors.Is(err, f.ErrInput) {
			t.Fatalf("expected ErrInput for Add target %q, got %v", badTarget, err)
		}
		if _, err := x.s.CourseReferences(ctx, "alice", []string{badTarget}); !errors.Is(err, f.ErrInput) {
			t.Fatalf("expected ErrInput for CourseReferences target %q, got %v", badTarget, err)
		}
	}
	refs, err := x.s.CourseReferences(ctx, "alice", []string{"course,1", "missing"})
	if err != nil || len(refs) != 1 || refs[0] != "course,1" {
		t.Fatalf("unexpected CourseReferences: %v, %v", refs, err)
	}

	// A-13 Part 1: Idempotency-Key must be 1..128 printable ASCII (0x21..0x7E); folder names normalize NFC and reject Cf/Zl/Zp/Cs.
	for _, badKey := range []string{"", "has space", "中文键", "\x7f", strings.Repeat("k", 129)} {
		if _, err := x.s.CreateFolder(ctx, "alice", badKey, "Valid"); !errors.Is(err, f.ErrInput) {
			t.Fatalf("expected ErrInput for Idempotency-Key %q, got %v", badKey, err)
		}
	}
	for _, badName := range []string{"\u200B", "RTL\u202Espoof", "line\u2028sep", "para\u2029sep"} {
		if _, err := x.s.CreateFolder(ctx, "alice", "bad-name-key", badName); !errors.Is(err, f.ErrInput) {
			t.Fatalf("expected ErrInput for folder name %q, got %v", badName, err)
		}
	}
	nfcFolder, err := x.s.CreateFolder(ctx, "alice", "nfd-create", "Cafe\u0301")
	if err != nil || nfcFolder.Name != "Café" {
		t.Fatalf("expected NFC normalized folder name 'Café', got %+v (err=%v)", nfcFolder, err)
	}
	if _, err := x.s.CreateFolder(ctx, "alice", "nfc-dup", "Café"); !errors.Is(err, f.ErrConflict) {
		t.Fatalf("expected ErrConflict for canonically equivalent folder name, got %v", err)
	}

	// A-13 Part 2: HTTP POST non-JSON Content-Type & duplicate single-value query parameters must return 400.
	rawReq := func(method, target, contentType, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Test-Session", "alice")
		r.Header.Set("Idempotency-Key", "raw-req-key")
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := rawReq("POST", "/api/v1/favorites/folders", "", `{"name":"missing"}`); w.Code != 400 {
		t.Errorf("missing Content-Type returned %d", w.Code)
	}
	var stored int
	if err := x.db.QueryRow("SELECT count(*) FROM favorites.folders WHERE name='missing'").Scan(&stored); err != nil || stored != 0 {
		t.Fatal(stored, err)
	}
	if w := rawReq("POST", "/api/v1/favorites/folders", "text/plain", `{"name":"Plain"}`); w.Code != 400 {
		t.Fatalf("expected 400 for text/plain POST, got %d", w.Code)
	}
	if w := rawReq("POST", "/api/v1/favorites/folders", "application/json; charset=utf-8", `{"name":"UTF8JSON"}`); w.Code != 200 {
		t.Fatalf("expected 200 for application/json; charset=utf-8, got %d %s", w.Code, w.Body.String())
	}
	for _, dupPath := range []string{
		"/api/v1/favorites/items?kind=course&kind=invalid",
		"/api/v1/favorites/items?limit=10&limit=20",
		"/api/v1/favorites/items?sort=newest&sort=name",
		"/api/v1/favorites/folders?limit=10&limit=20",
		"/api/v1/favorites/folders?after=a&after=b",
	} {
		if w := rawReq("GET", dupPath, "", ""); w.Code != 400 {
			t.Fatalf("expected 400 for duplicate query param %s, got %d", dupPath, w.Code)
		}
	}

	// A-13 Part 3: Sub-context 3s timeout in Source.Resolve wraps as ErrUnavailable when outer ctx is still active,
	// and nil postgres.Repository / nil DB returns ErrUnavailable without panicking.
	x.source.state.Store(3)
	if _, err := x.s.Add(ctx, "alice", "subtimeout-add", f.Add{Kind: "resource", TargetID: "res-subtimeout"}); !errors.Is(err, f.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable on Add sub-timeout, got %v", err)
	}
	if _, _, err := x.s.CurrentActivity(ctx, "alice", f.ActivityID("course", "course,1", x.s.Now())); !errors.Is(err, f.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable on CurrentActivity sub-timeout, got %v", err)
	}

	var nilRepo *postgres.Repository
	emptyRepo := &postgres.Repository{}
	for _, r := range []*postgres.Repository{nilRepo, emptyRepo} {
		if err := r.Ready(ctx); !errors.Is(err, f.ErrUnavailable) {
			t.Fatalf("expected ErrUnavailable from Ready on nil repo/DB, got %v", err)
		}
		if err := r.CheckRuntime(ctx); !errors.Is(err, f.ErrUnavailable) {
			t.Fatalf("expected ErrUnavailable from CheckRuntime on nil repo/DB, got %v", err)
		}
		if _, err := r.PendingOutbox(ctx, 10, nil); !errors.Is(err, f.ErrUnavailable) {
			t.Fatalf("expected ErrUnavailable from PendingOutbox on nil repo/DB, got %v", err)
		}
	}
	if err := postgres.Migrate(ctx, nil); !errors.Is(err, f.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable from Migrate(nil), got %v", err)
	}
}

func TestFolderNameNormalizationReplaysPreUpgradeReceipts(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	raw := "Cafe\u0301"
	old := f.Folder{ID: "preserved", Name: raw, Revision: 3}
	result, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	rename := f.Rename{Name: raw, Revision: 2}
	err = x.repo.Transaction(ctx, "owner", func(tx f.Transaction) error {
		for _, c := range []struct {
			key, op string
			body    any
		}{{"legacy-create", "folder.create", raw}, {"legacy-rename", "folder.rename", []any{old.ID, rename}}} {
			b, e := json.Marshal([]any{c.op, c.body})
			if e != nil {
				return e
			}
			if e = tx.SaveReplay(c.key, fmt.Sprintf("%x", sha256.Sum256(b)), result); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := x.s.CreateFolder(ctx, "owner", "legacy-create", raw)
	if err != nil || created != old {
		t.Fatalf("create replay: %+v %v", created, err)
	}
	renamed, err := x.s.RenameFolder(ctx, "owner", "legacy-rename", old.ID, rename)
	if err != nil || renamed != old {
		t.Fatalf("rename replay: %+v %v", renamed, err)
	}
	if _, err = x.s.CreateFolder(ctx, "owner", "legacy-create", "Different"); !errors.Is(err, f.ErrConflict) {
		t.Fatalf("different request: %v", err)
	}
	var writes int
	if err = x.db.QueryRow("SELECT count(*) FROM favorites.events WHERE owner_key='owner'").Scan(&writes); err != nil || writes != 0 {
		t.Fatalf("replay emitted %d events: %v", writes, err)
	}
}
