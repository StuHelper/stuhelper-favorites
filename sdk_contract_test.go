package favorites_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/StuHelper/stuhelper-favorites/postgres"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/httpapi"
	"github.com/StuHelper/stuhelper-favorites/sdk"
)

func TestGeneratedSDKRoutesCourseReferencesTimeoutAndOutboxLag(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	x.s.Now = func() time.Time { return now }

	// 1. OutboxLag starts at zero when there are no unsent events.
	lag, err := x.s.OutboxLag(ctx)
	if err != nil || lag.PendingCount != 0 || !lag.OldestPendingAt.IsZero() || lag.OldestAge != 0 {
		t.Fatal("expected empty outbox lag", lag, err)
	}

	// 2. End-to-end generated Go SDK contract test across all HTTP routes, including commas and slashes.
	srv := httptest.NewServer(httpapi.Handler{
		Service: x.s,
		Authenticate: func(r *http.Request) (string, error) {
			return r.Header.Get("Test-Session"), nil
		},
	})
	defer srv.Close()

	clientFor := func(owner string) *sdk.ClientWithResponses {
		c, err := sdk.NewClientWithResponses(srv.URL, sdk.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Test-Session", owner)
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	alice := clientFor("alice")
	bob := clientFor("bob")

	foldersRes, err := alice.ListFoldersWithResponse(ctx, &sdk.ListFoldersParams{})
	if err != nil || foldersRes.StatusCode() != http.StatusOK || foldersRes.JSON200 == nil || len(foldersRes.JSON200.Items) != 1 {
		t.Fatal(err, foldersRes)
	}
	createdFolder, err := alice.CreateFolderWithResponse(ctx, &sdk.CreateFolderParams{IdempotencyKey: "sdk-f1"}, sdk.Name{Name: "SDK测试夹"})
	if err != nil || createdFolder.StatusCode() != http.StatusOK || createdFolder.JSON200 == nil {
		t.Fatal(err, createdFolder)
	}
	renamedFolder, err := alice.RenameFolderWithResponse(ctx, createdFolder.JSON200.Id, &sdk.RenameFolderParams{IdempotencyKey: "sdk-rn1"}, sdk.Rename{
		Name:     "SDK改名夹",
		Revision: createdFolder.JSON200.Revision,
	})
	if err != nil || renamedFolder.StatusCode() != http.StatusOK || renamedFolder.JSON200 == nil || renamedFolder.JSON200.Name != "SDK改名夹" {
		t.Fatal(err, renamedFolder)
	}

	// Add courses with literal comma and slash in target_id and verify SDK form explode=false encoding (%2C / %2F).
	addComma, err := alice.AddFavoriteWithResponse(ctx, &sdk.AddFavoriteParams{IdempotencyKey: "sdk-add-comma"}, sdk.Add{
		Kind:     sdk.AddKindCourse,
		TargetId: "course,1",
	})
	if err != nil || addComma.StatusCode() != http.StatusOK || addComma.JSON200 == nil {
		t.Fatal(err, addComma)
	}
	now = now.Add(65 * time.Second)
	addSlash, err := alice.AddFavoriteWithResponse(ctx, &sdk.AddFavoriteParams{IdempotencyKey: "sdk-add-slash"}, sdk.Add{
		Kind:     sdk.AddKindCourse,
		TargetId: "dept/course-2",
	})
	if err != nil || addSlash.StatusCode() != http.StatusOK || addSlash.JSON200 == nil {
		t.Fatal(err, addSlash)
	}
	if _, err := bob.AddFavoriteWithResponse(ctx, &sdk.AddFavoriteParams{IdempotencyKey: "bob-course"}, sdk.Add{
		Kind:     sdk.AddKindCourse,
		TargetId: "bob-only-course",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.AddFavoriteWithResponse(ctx, &sdk.AddFavoriteParams{IdempotencyKey: "sdk-add-teacher"}, sdk.Add{
		Kind:     sdk.AddKindTeacher,
		TargetId: "course,1",
	}); err != nil {
		t.Fatal(err)
	}

	kindCourse := sdk.ListFavoritesParamsKindCourse
	unicodeIDs := make([]string, 100)
	for i := range unicodeIDs {
		unicodeIDs[i] = strings.Repeat("😀", 197) + fmt.Sprintf("%03d", i)
	}
	unicodePage, err := alice.ListFavoritesWithResponse(ctx, &sdk.ListFavoritesParams{Kind: &kindCourse, TargetIds: &unicodeIDs})
	if err != nil || unicodePage.JSON200 == nil {
		t.Fatalf("long Unicode targets: %+v %v", unicodePage, err)
	}

	targets := []string{"course,1", "dept/course-2", "absent"}
	listRes, err := alice.ListFavoritesWithResponse(ctx, &sdk.ListFavoritesParams{
		Kind:      &kindCourse,
		TargetIds: &targets,
	})
	if err != nil || listRes.StatusCode() != http.StatusOK || listRes.JSON200 == nil || len(listRes.JSON200.Items) != 2 {
		t.Fatal("SDK target_ids with comma/slash failed", err, listRes)
	}

	// Move & Remove via SDK, plus path segment containing slash (%2F).
	movedRes, err := alice.MoveFavoriteWithResponse(ctx, addComma.JSON200.Id, &sdk.MoveFavoriteParams{IdempotencyKey: "sdk-mv"}, sdk.Move{
		FolderId: renamedFolder.JSON200.Id,
		Revision: addComma.JSON200.Revision,
	})
	if err != nil || movedRes.StatusCode() != http.StatusOK || movedRes.JSON200 == nil || movedRes.JSON200.FolderId != renamedFolder.JSON200.Id {
		t.Fatal(err, movedRes)
	}
	// Seed a custom folder and item with '/' in their IDs to verify URL-escaped path segments via SDK.
	if _, err := x.db.Exec(`INSERT INTO favorites.folders(owner_key,id,name,is_default,revision) VALUES('alice','folder/slash','斜杠夹',false,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := x.db.Exec(`INSERT INTO favorites.items(owner_key,id,kind,target_id,canonical_target_id,title,folder_id,revision,created_at) VALUES('alice','item/slash','resource','res-slash','res-slash','Title','folder/slash',1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	rmSlash, err := alice.RemoveFavoriteWithResponse(ctx, "item/slash", &sdk.RemoveFavoriteParams{IdempotencyKey: "sdk-rm-slash"}, sdk.Revision{Revision: 1})
	if err != nil || rmSlash.StatusCode() != http.StatusOK || rmSlash.JSON200 == nil || !rmSlash.JSON200.Removed {
		t.Fatal("escaped slash item ID failed", err, rmSlash)
	}
	delSlashFolder, err := alice.DeleteFolderWithResponse(ctx, "folder/slash", &sdk.DeleteFolderParams{IdempotencyKey: "sdk-del-slash"}, sdk.Revision{Revision: 1})
	if err != nil || delSlashFolder.StatusCode() != http.StatusOK || delSlashFolder.JSON200 == nil || !delSlashFolder.JSON200.Removed {
		t.Fatal("escaped slash folder ID failed", err, delSlashFolder)
	}
	delFolder, err := alice.DeleteFolderWithResponse(ctx, renamedFolder.JSON200.Id, &sdk.DeleteFolderParams{IdempotencyKey: "sdk-del-f1"}, sdk.Revision{Revision: renamedFolder.JSON200.Revision})
	if err != nil || delFolder.StatusCode() != http.StatusOK || delFolder.JSON200 == nil || !delFolder.JSON200.Removed {
		t.Fatal(err, delFolder)
	}

	// 3. CourseReferences: ownership isolation, sorted unique output, teacher/resource exclusion, and boundary validation.
	refs, err := x.s.CourseReferences(ctx, "alice", []string{"dept/course-2", "course,1", "course,1", "bob-only-course", "missing"})
	if err != nil || !slices.Equal(refs, []string{"course,1", "dept/course-2"}) {
		t.Fatal("unexpected CourseReferences", refs, err)
	}
	for _, badIDs := range [][]string{
		nil,
		{},
		{""},
		{" course,1"},
		{"course,1 "},
		{"bad\x00"},
		{strings.Repeat("x", 201)},
		make([]string, 101),
	} {
		if _, err := x.s.CourseReferences(ctx, "alice", badIDs); !errors.Is(err, f.ErrInput) {
			t.Fatal("expected ErrInput for CourseReferences", badIDs, err)
		}
	}
	if _, err := x.s.CourseReferences(ctx, "", []string{"course,1"}); !errors.Is(err, f.ErrUnauthenticated) {
		t.Fatal("expected ErrUnauthenticated", err)
	}
	if _, err := (&f.Service{}).CourseReferences(ctx, "alice", []string{"course,1"}); !errors.Is(err, f.ErrUnavailable) {
		t.Fatal("expected ErrUnavailable for nil repo", err)
	}

	// 4. Source.Resolve child timeout (state = 3) maps to ErrUnavailable when parent ctx is still active.
	x.source.state.Store(3)
	if _, err := x.s.Add(ctx, "alice", "timeout-add", f.Add{Kind: "course", TargetID: "slow-course"}); !errors.Is(err, f.ErrUnavailable) {
		t.Fatal("expected ErrUnavailable on Source.Resolve child timeout in Add, got", err)
	}
	if _, _, err := x.s.CurrentActivity(ctx, "alice", f.ActivityID("course", "course,1", now)); !errors.Is(err, f.ErrUnavailable) {
		t.Fatal("expected ErrUnavailable on Source.Resolve child timeout in CurrentActivity, got", err)
	}
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := x.s.Add(cancelledCtx, "alice", "cancelled-add", f.Add{Kind: "course", TargetID: "slow-course"}); !errors.Is(err, context.Canceled) {
		t.Fatal("expected parent context.Canceled preserved, got", err)
	}
	x.source.state.Store(0)

	// 5. OutboxLag tracks pending count and oldest age, advancing as oldest events are confirmed.
	now = now.Add(25 * time.Hour)
	lag, err = x.s.OutboxLag(ctx)
	if err != nil || lag.PendingCount != 4 || lag.OldestAge < 25*time.Hour {
		t.Fatal("unexpected outbox lag before confirmation", lag, err)
	}
	pending, err := x.repo.PendingOutbox(ctx, 100, nil)
	if err != nil || len(pending) != 4 {
		t.Fatal(pending, err)
	}
	for _, ev := range pending {
		if err := x.repo.ConfirmOutbox(ctx, ev.ID); err != nil {
			t.Fatal(err)
		}
	}
	lagAfter, err := x.s.OutboxLag(ctx)
	if err != nil || lagAfter.PendingCount != 0 || lagAfter.OldestAge != 0 || !lagAfter.OldestPendingAt.IsZero() {
		t.Fatal("expected zero lag after confirming all outbox events", lagAfter, err)
	}
}

func TestOutboxLagRetainsEnvelopeTimeAfterMigration(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := x.db.Exec(`ALTER TABLE favorites.outbox DROP COLUMN created_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := x.db.Exec(`INSERT INTO favorites.outbox(id,data) VALUES('old',jsonb_build_object('time',$1::timestamptz))`, at); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := postgres.Migrate(ctx, x.db); err != nil {
			t.Fatal(err)
		}
		lag, err := x.s.OutboxLag(ctx)
		if err != nil || lag.PendingCount != 1 || !lag.OldestPendingAt.Equal(at) || lag.OldestAge < 24*time.Hour {
			t.Fatalf("pending age after migration: %+v %v", lag, err)
		}
	}
}
