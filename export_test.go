package favorites_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/httpapi"
	"github.com/StuHelper/stuhelper-favorites/sdk"
)

func TestExportOwnFavoritesBoundedPaginationFolderFilterAndIsolation(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	x.s.Now = func() time.Time { return now }

	// 1. Uninitialized account export is strictly read-only and does not insert owner/folder rows.
	emptyExport, err := x.s.Export(ctx, "fresh-user", f.ExportFilter{Limit: 200})
	if err != nil || len(emptyExport.Folders) != 0 || len(emptyExport.Items) != 0 {
		t.Fatal(emptyExport, err)
	}
	var ownerRows int
	if err := x.db.QueryRow(`SELECT count(*) FROM favorites.owners WHERE owner_key='fresh-user'`).Scan(&ownerRows); err != nil || ownerRows != 0 {
		t.Fatal("export created side-effect rows", ownerRows, err)
	}
	if _, err := x.s.Export(ctx, "fresh-user", f.ExportFilter{FolderID: "missing", Limit: 20}); !errors.Is(err, f.ErrNotFound) {
		t.Fatal("expected not_found for missing folder on uninitialized owner", err)
	}

	// 2. Seed Alice and Bob favorites (including alias course -> canonical target path).
	pack, err := x.s.CreateFolder(ctx, "alice", "f-pack", "选课复习包")
	if err != nil {
		t.Fatal(err)
	}
	bobFolder, err := x.s.CreateFolder(ctx, "bob", "bob-f", "Bob夹")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.Add(ctx, "bob", "bob-1", f.Add{Kind: "course", TargetID: "secret-course", FolderID: bobFolder.ID}); err != nil {
		t.Fatal(err)
	}

	for _, seed := range []struct {
		key, kind, target, folder string
	}{
		{"a1", "course", "old-alias", pack.ID},
		{"a2", "teacher", "prof/zhang", pack.ID},
		{"a3", "resource", "exam-2025", ""},
	} {
		now = now.Add(time.Minute)
		if _, err := x.s.Add(ctx, "alice", seed.key, f.Add{Kind: seed.kind, TargetID: seed.target, FolderID: seed.folder}); err != nil {
			t.Fatal(err)
		}
	}

	// Revoke content admission: personal export still works without querying upstream Source.Resolve.
	x.source.state.Store(1)
	calls := x.source.calls.Load()

	srv := httptest.NewServer(httpapi.Handler{
		Service: x.s,
		Authenticate: func(r *http.Request) (string, error) {
			return r.Header.Get("Test-Session"), nil
		},
	})
	defer srv.Close()

	clientFor := func(owner string) *sdk.ClientWithResponses {
		c, err := sdk.NewClientWithResponses(srv.URL, sdk.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			if owner != "" {
				req.Header.Set("Test-Session", owner)
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	alice := clientFor("alice")
	bob := clientFor("bob")
	anon := clientFor("")

	// Paginate Alice's full export with limit=2 via SDK.
	limit := 2
	page1, err := alice.ExportFavoritesWithResponse(ctx, &sdk.ExportFavoritesParams{Limit: &limit})
	if err != nil || page1.StatusCode() != http.StatusOK || page1.JSON200 == nil || len(page1.JSON200.Folders) != 2 || len(page1.JSON200.Items) != 2 || page1.JSON200.Next == nil {
		t.Fatal(err, page1)
	}
	page2, err := alice.ExportFavoritesWithResponse(ctx, &sdk.ExportFavoritesParams{Limit: &limit, After: page1.JSON200.Next})
	if err != nil || page2.StatusCode() != http.StatusOK || page2.JSON200 == nil || len(page2.JSON200.Items) != 1 || page2.JSON200.Next != nil {
		t.Fatal(err, page2)
	}
	if x.source.calls.Load() != calls {
		t.Fatal("export unexpectedly called upstream Source.Resolve")
	}

	allItems := append(page1.JSON200.Items, page2.JSON200.Items...)
	// Descending created_at order: exam-2025 (default ""), prof/zhang ("选课复习包"), old-alias ("选课复习包", canonical "course").
	if allItems[0].TargetId != "exam-2025" || allItems[0].TargetPath != "/resources/items/exam-2025" || allItems[0].FolderName != "" {
		t.Fatal("unexpected resource export item", allItems[0])
	}
	if allItems[1].TargetId != "prof/zhang" || allItems[1].TargetPath != "" || allItems[1].FolderName != "选课复习包" {
		t.Fatal("unexpected teacher export item", allItems[1])
	}
	if allItems[2].TargetId != "old-alias" || allItems[2].CanonicalTargetId != "course" || allItems[2].TargetPath != "" || allItems[2].FolderName != "选课复习包" {
		t.Fatal("unexpected alias course export item", allItems[2])
	}

	x.source.state.Store(0)
	visible, err := x.s.Export(ctx, "alice", f.ExportFilter{Limit: 20})
	if err != nil || len(visible.Items) != 3 || visible.Items[1].TargetPath != "/teachers/38271" || visible.Items[2].TargetPath != "/courses/B240012012" {
		t.Fatal(visible, err)
	}
	x.source.state.Store(2)
	if _, err = x.s.Export(ctx, "alice", f.ExportFilter{Limit: 20}); !errors.Is(err, f.ErrUnavailable) {
		t.Fatal(err)
	}
	x.source.state.Store(1)

	// Folder-scoped & kind-scoped export.
	kindCourse := sdk.ExportFavoritesParamsKindCourse
	filtered, err := alice.ExportFavoritesWithResponse(ctx, &sdk.ExportFavoritesParams{
		FolderId: &pack.ID,
		Kind:     &kindCourse,
	})
	if err != nil || filtered.StatusCode() != http.StatusOK || filtered.JSON200 == nil || len(filtered.JSON200.Folders) != 1 || len(filtered.JSON200.Items) != 1 || filtered.JSON200.Items[0].TargetId != "old-alias" {
		t.Fatal(err, filtered)
	}

	// More folders than items: empty folders must be exported once within the same page bound.
	for _, name := range []string{"empty1", "empty2", "empty3"} {
		if _, err := x.s.CreateFolder(ctx, "alice", "new-"+name, name); err != nil {
			t.Fatal(err)
		}
	}
	one := 1
	var cursor *string
	seenFolders, seenItems := map[string]bool{}, map[string]bool{}
	for pages := 0; ; pages++ {
		if pages >= 6 {
			t.Fatal("export cursor did not finish")
		}
		page, err := alice.ExportFavoritesWithResponse(ctx, &sdk.ExportFavoritesParams{Limit: &one, After: cursor})
		if err != nil || page.JSON200 == nil {
			t.Fatalf("export page %d: %v %+v", pages, err, page)
		}
		data := page.JSON200
		if len(data.Folders) > 1 || len(data.Items) > 1 {
			t.Fatalf("unbounded export: %d folders, %d items", len(data.Folders), len(data.Items))
		}
		for _, folder := range data.Folders {
			if seenFolders[folder.Id] {
				t.Fatalf("repeated folder %s", folder.Id)
			}
			seenFolders[folder.Id] = true
		}
		for _, item := range data.Items {
			if seenItems[item.Id] {
				t.Fatalf("repeated item %s", item.Id)
			}
			seenItems[item.Id] = true
		}
		cursor = data.Next
		if cursor == nil {
			break
		}
	}
	if len(seenFolders) != 5 || len(seenItems) != 3 {
		t.Fatalf("export lost data: %v %v", seenFolders, seenItems)
	}

	// Isolation & cursor/boundary validation:
	// - Cross-owner cursor or filter change -> 400
	// - Cross-owner folder_id -> 404
	// - Unauthenticated -> 401
	// - Limit > 500 -> 400
	// - Closed owner -> 403
	if crossCursor, err := bob.ExportFavoritesWithResponse(ctx, &sdk.ExportFavoritesParams{After: page1.JSON200.Next}); err != nil || crossCursor.StatusCode() != http.StatusBadRequest {
		t.Fatal(err, crossCursor)
	}
	if filterChangedCursor, err := alice.ExportFavoritesWithResponse(ctx, &sdk.ExportFavoritesParams{Kind: &kindCourse, After: page1.JSON200.Next}); err != nil || filterChangedCursor.StatusCode() != http.StatusBadRequest {
		t.Fatal(err, filterChangedCursor)
	}
	if foreignFolder, err := alice.ExportFavoritesWithResponse(ctx, &sdk.ExportFavoritesParams{FolderId: &bobFolder.ID}); err != nil || foreignFolder.StatusCode() != http.StatusNotFound {
		t.Fatal(err, foreignFolder)
	}
	if unauth, err := anon.ExportFavoritesWithResponse(ctx, &sdk.ExportFavoritesParams{}); err != nil || unauth.StatusCode() != http.StatusUnauthorized {
		t.Fatal(err, unauth)
	}
	overLimit := 501
	if tooBig, err := alice.ExportFavoritesWithResponse(ctx, &sdk.ExportFavoritesParams{Limit: &overLimit}); err != nil || tooBig.StatusCode() != http.StatusBadRequest {
		t.Fatal(err, tooBig)
	}
	if err := x.s.CloseOwner(ctx, "bob", "44444444-4444-4444-8444-444444444444"); err != nil {
		t.Fatal(err)
	}
	if closedRes, err := bob.ExportFavoritesWithResponse(ctx, &sdk.ExportFavoritesParams{}); err != nil || closedRes.StatusCode() != http.StatusForbidden || closedRes.JSON403 == nil || closedRes.JSON403.Reason == nil || *closedRes.JSON403.Reason != sdk.OwnerClosed {
		t.Fatal(err, closedRes)
	}
}

func (s *source) VisibleAddresses(ctx context.Context, owner, kind string, ids []string) (map[string]string, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("no timeout")
	}
	switch s.state.Load() {
	case 1:
		return nil, f.ErrForbidden
	case 2:
		return nil, f.ErrUnavailable
	}
	out := map[string]string{}
	for _, id := range ids {
		switch id {
		case "course":
			out[id] = "B240012012"
		case "prof/zhang":
			out[id] = "38271"
		}
	}
	return out, nil
}
