package favorites_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/httpapi"
	"github.com/StuHelper/stuhelper-favorites/sdk"
)

func TestTargetSetAndRemoveSwitchFoldersIdempotentlyWithoutDuplicateActivity(t *testing.T) {
	x := setup(t)
	ctx := context.Background()

	folders, err := x.s.ListFolders(ctx, "alice", "", 10)
	if err != nil || len(folders.Items) != 1 {
		t.Fatal(folders, err)
	}
	def := folders.Items[0]
	backup, err := x.s.CreateFolder(ctx, "alice", "create-backup", "下学期备选")
	if err != nil {
		t.Fatal(err)
	}
	bobFolder, err := x.s.CreateFolder(ctx, "bob", "bob-folder", "Bob夹")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.Add(ctx, "bob", "bob-course", f.Add{Kind: "course", TargetID: "cs101", FolderID: bobFolder.ID}); err != nil {
		t.Fatal(err)
	}

	created, err := x.s.SetTarget(ctx, "alice", "set-1", f.Add{Kind: "course", TargetID: "cs101"})
	if err != nil || created.FolderID != def.ID || created.Revision != 1 {
		t.Fatal(created, err)
	}
	actBefore, err := x.repo.ReadActivity(ctx, f.ActivityID("course", "cs101", x.s.Now()))
	if err != nil || actBefore.Metrics["favorites"] != 2 {
		t.Fatal(actBefore, err)
	}

	// Revoke content admission: own existing favorite can still switch folders or be removed.
	x.source.state.Store(1)
	calls := x.source.calls.Load()

	same, err := x.s.SetTarget(ctx, "alice", "set-noop", f.Add{Kind: "course", TargetID: "cs101"})
	if err != nil || same.ID != created.ID || same.Revision != 1 || same.FolderID != def.ID {
		t.Fatal(same, err)
	}
	moved, err := x.s.SetTarget(ctx, "alice", "set-move", f.Add{Kind: "course", TargetID: "cs101", FolderID: backup.ID})
	if err != nil || moved.ID != created.ID || moved.FolderID != backup.ID || moved.Revision != 2 {
		t.Fatal(moved, err)
	}
	backToDefault, err := x.s.SetTarget(ctx, "alice", "set-default", f.Add{Kind: "course", TargetID: "cs101"})
	if err != nil || backToDefault.ID != created.ID || backToDefault.FolderID != def.ID || backToDefault.Revision != 3 {
		t.Fatal(backToDefault, err)
	}
	if x.source.calls.Load() != calls {
		t.Fatal("existing target set queried upstream source")
	}
	actAfterMove, err := x.repo.ReadActivity(ctx, f.ActivityID("course", "cs101", x.s.Now()))
	if err != nil || actAfterMove.Revision != actBefore.Revision || actAfterMove.Metrics["favorites"] != 2 {
		t.Fatal("folder switch altered public activity", actAfterMove, err)
	}

	// Unowned or missing folder_id must fail even when target is already favorited.
	if _, err := x.s.SetTarget(ctx, "alice", "set-foreign-folder", f.Add{Kind: "course", TargetID: "cs101", FolderID: bobFolder.ID}); !errors.Is(err, f.ErrNotFound) {
		t.Fatal("expected not_found for foreign folder", err)
	}
	// New target still enforces current admission.
	if _, err := x.s.SetTarget(ctx, "alice", "set-denied", f.Add{Kind: "course", TargetID: "cs202"}); !errors.Is(err, f.ErrForbidden) {
		t.Fatal("expected forbidden for unowned new target without admission", err)
	}

	// Remove by target is idempotent: first call removes, second new key returns removed=false, replay returns original removed=true.
	rem1, err := x.s.RemoveTarget(ctx, "alice", "rm-1", f.Target{Kind: "course", TargetID: "cs101"})
	if err != nil || !rem1.Removed || rem1.ID != created.ID {
		t.Fatal(rem1, err)
	}
	rem2, err := x.s.RemoveTarget(ctx, "alice", "rm-2", f.Target{Kind: "course", TargetID: "cs101"})
	if err != nil || rem2.Removed || rem2.ID != "" {
		t.Fatal(rem2, err)
	}
	replayRem1, err := x.s.RemoveTarget(ctx, "alice", "rm-1", f.Target{Kind: "course", TargetID: "cs101"})
	if err != nil || !replayRem1.Removed || replayRem1.ID != created.ID {
		t.Fatal(replayRem1, err)
	}
	if _, err := x.s.RemoveTarget(ctx, "alice", "rm-1", f.Target{Kind: "course", TargetID: "other"}); !errors.Is(err, f.ErrConflict) {
		t.Fatal("expected idempotency conflict", err)
	}

	// Bob's favorite for the same target remains intact, and public activity only decremented Alice's 1.
	bobList, err := x.s.List(ctx, "bob", f.Filter{Kind: "course", TargetIDs: []string{"cs101"}, Limit: 10})
	if err != nil || len(bobList.Items) != 1 {
		t.Fatal(bobList, err)
	}
	actAfterRemove, err := x.repo.ReadActivity(ctx, f.ActivityID("course", "cs101", x.s.Now()))
	if err != nil || actAfterRemove.Metrics["favorites"] != 1 {
		t.Fatal(actAfterRemove, err)
	}

	// Input boundary checks.
	for _, bad := range []f.Add{
		{Kind: "invalid", TargetID: "cs101"},
		{Kind: "course", TargetID: " cs101"},
		{Kind: "course", TargetID: "cs101 "},
		{Kind: "course", TargetID: ""},
		{Kind: "course", TargetID: "cs101", FolderID: "bad\x00"},
	} {
		if _, err := x.s.SetTarget(ctx, "alice", "bad", bad); !errors.Is(err, f.ErrInput) {
			t.Fatal(bad, err)
		}
	}
	if _, err := x.s.RemoveTarget(ctx, "alice", "bad-rm", f.Target{Kind: "course", TargetID: " cs101"}); !errors.Is(err, f.ErrInput) {
		t.Fatal(err)
	}

	// Verify HTTP routes via generated Go SDK client.
	x.source.state.Store(0)
	alias, err := x.s.Add(ctx, "alias-owner", "add", f.Add{Kind: "course", TargetID: "old-alias"})
	if err != nil {
		t.Fatal(err)
	}
	folder, err := x.s.CreateFolder(ctx, "alias-owner", "folder", "Canonical")
	if err != nil {
		t.Fatal(err)
	}
	movedAlias, err := x.s.SetTarget(ctx, "alias-owner", "set-canonical", f.Add{Kind: "course", TargetID: "course", FolderID: folder.ID})
	if err != nil || movedAlias.ID != alias.ID || movedAlias.FolderID != folder.ID {
		t.Fatal(movedAlias, err)
	}
	gone, err := x.s.RemoveTarget(ctx, "alias-owner", "rm-canonical", f.Target{Kind: "course", TargetID: "course"})
	if err != nil || !gone.Removed || gone.ID != alias.ID {
		t.Fatal(gone, err)
	}

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
	setRes, err := client.SetTargetFavoriteWithResponse(ctx, &sdk.SetTargetFavoriteParams{IdempotencyKey: "sdk-set-1"}, sdk.Add{
		Kind:     sdk.AddKindResource,
		TargetId: "res-1",
		FolderId: &backup.ID,
	})
	if err != nil || setRes.StatusCode() != http.StatusOK || setRes.JSON200 == nil || setRes.JSON200.FolderId != backup.ID {
		t.Fatal(err, setRes)
	}
	rmRes, err := client.RemoveTargetFavoriteWithResponse(ctx, &sdk.RemoveTargetFavoriteParams{IdempotencyKey: "sdk-rm-1"}, sdk.Target{
		Kind:     sdk.TargetKindResource,
		TargetId: "res-1",
	})
	if err != nil || rmRes.StatusCode() != http.StatusOK || rmRes.JSON200 == nil || !rmRes.JSON200.Removed || rmRes.JSON200.Id != setRes.JSON200.Id {
		t.Fatal(err, rmRes)
	}
	rmAgain, err := client.RemoveTargetFavoriteWithResponse(ctx, &sdk.RemoveTargetFavoriteParams{IdempotencyKey: "sdk-rm-2"}, sdk.Target{
		Kind:     sdk.TargetKindResource,
		TargetId: "res-1",
	})
	if err != nil || rmAgain.StatusCode() != http.StatusOK || rmAgain.JSON200 == nil || rmAgain.JSON200.Removed || rmAgain.JSON200.Id != "" {
		t.Fatal(err, rmAgain)
	}
}
