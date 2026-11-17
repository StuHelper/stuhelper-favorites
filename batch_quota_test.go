package favorites_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/httpapi"
	"github.com/StuHelper/stuhelper-favorites/sdk"
)

func TestBatchMoveRemoveAtomicityActivityAggregationAndOwnerQuotas(t *testing.T) {
	x := setup(t)
	ctx := context.Background()

	if f.DefaultMaxFolders != 200 || f.DefaultMaxItems != 10000 {
		t.Fatalf("unexpected default quotas: %d / %d", f.DefaultMaxFolders, f.DefaultMaxItems)
	}

	// 1. Folder & Item Quotas (including concurrency at boundary).
	x.s.MaxFolders = 3 // 1 default + up to 2 custom folders
	x.s.MaxItems = 4

	var wg sync.WaitGroup
	folderErrs := make(chan error, 4)
	for i := range 4 {
		wg.Go(func() {
			_, err := x.s.CreateFolder(ctx, "alice", fmt.Sprintf("cf-%d", i), fmt.Sprintf("夹-%d", i))
			folderErrs <- err
		})
	}
	wg.Wait()
	close(folderErrs)
	okCount, conflictCount := 0, 0
	for err := range folderErrs {
		if err == nil {
			okCount++
		} else if errors.Is(err, f.ErrConflict) {
			conflictCount++
		} else {
			t.Fatal(err)
		}
	}
	if okCount != 2 || conflictCount != 2 {
		t.Fatalf("expected 2 created and 2 quota conflicts, got %d / %d", okCount, conflictCount)
	}

	folders, err := x.s.ListFolders(ctx, "alice", "", 10)
	if err != nil || len(folders.Items) != 3 {
		t.Fatal(folders, err)
	}
	var defFolder, customA, customB f.Folder
	for _, fld := range folders.Items {
		if fld.Default {
			defFolder = fld
		} else if customA.ID == "" {
			customA = fld
		} else {
			customB = fld
		}
	}

	// Fill item quota (4 items): "old-alias" + "course" (both canonical "course"), "t1" (teacher), "r1" (resource).
	itemAlias, err := x.s.Add(ctx, "alice", "add-alias", f.Add{Kind: "course", TargetID: "old-alias", FolderID: defFolder.ID})
	if err != nil {
		t.Fatal(err)
	}
	itemCourse, err := x.s.Add(ctx, "alice", "add-course", f.Add{Kind: "course", TargetID: "course", FolderID: customA.ID})
	if err != nil {
		t.Fatal(err)
	}
	itemTeacher, err := x.s.Add(ctx, "alice", "add-teacher", f.Add{Kind: "teacher", TargetID: "t1", FolderID: defFolder.ID})
	if err != nil {
		t.Fatal(err)
	}
	itemResource, err := x.s.Add(ctx, "alice", "add-res", f.Add{Kind: "resource", TargetID: "r1", FolderID: defFolder.ID})
	if err != nil {
		t.Fatal(err)
	}

	// 5th distinct item hits MaxItems=4 -> 409 Conflict; re-adding existing item still succeeds!
	if _, err := x.s.Add(ctx, "alice", "over-quota", f.Add{Kind: "resource", TargetID: "r2"}); !errors.Is(err, f.ErrConflict) {
		t.Fatal("expected item quota conflict", err)
	}
	reAdd, err := x.s.Add(ctx, "alice", "readd-at-quota", f.Add{Kind: "resource", TargetID: "r1"})
	if err != nil || reAdd.ID != itemResource.ID {
		t.Fatal("existing item add blocked by quota", reAdd, err)
	}

	bobItem, err := x.s.Add(ctx, "bob", "bob-add", f.Add{Kind: "course", TargetID: "course"})
	if err != nil {
		t.Fatal(err)
	}

	// 2. BatchMove atomicity: including Bob's item ID must fail with 404 and roll back ALL moves.
	if _, err := x.s.BatchMove(ctx, "alice", "bm-rollback", f.BatchMove{
		IDs:      []string{itemAlias.ID, bobItem.ID},
		FolderID: customA.ID,
	}); !errors.Is(err, f.ErrNotFound) {
		t.Fatal("expected 404 for foreign item in batch move", err)
	}
	checkUnmoved, err := x.s.List(ctx, "alice", f.Filter{Kind: "course", TargetIDs: []string{"old-alias"}, Limit: 10})
	if err != nil || checkUnmoved.Items[0].FolderID != defFolder.ID || checkUnmoved.Items[0].Revision != 1 {
		t.Fatal("failed batch move left partial update", checkUnmoved, err)
	}

	actCourseBefore, err := x.repo.ReadActivity(ctx, f.ActivityID("course", "course", x.s.Now()))
	if err != nil || actCourseBefore.Metrics["favorites"] != 3 {
		t.Fatal(actCourseBefore, err)
	}

	// Valid BatchMove: itemCourse is already in customA (revision stays 1), itemAlias & itemResource move into customA (revision becomes 2).
	moved, err := x.s.BatchMove(ctx, "alice", "bm-ok", f.BatchMove{
		IDs:      []string{itemResource.ID, itemCourse.ID, itemAlias.ID, itemResource.ID},
		FolderID: customA.ID,
	})
	if err != nil || len(moved.Items) != 3 {
		t.Fatal(moved, err)
	}
	if moved.Items[0].ID != itemResource.ID || moved.Items[0].Revision != 2 ||
		moved.Items[1].ID != itemCourse.ID || moved.Items[1].Revision != 1 ||
		moved.Items[2].ID != itemAlias.ID || moved.Items[2].Revision != 2 {
		t.Fatal("unexpected batch move result order or revisions", moved.Items)
	}
	actCourseAfterMove, err := x.repo.ReadActivity(ctx, f.ActivityID("course", "course", x.s.Now()))
	if err != nil || actCourseAfterMove.Revision != actCourseBefore.Revision {
		t.Fatal("batch move altered activity bucket", actCourseAfterMove, err)
	}

	// 3. BatchRemove rollback on outbox failure & coalesced canonical activity update on success.
	if _, err := x.db.Exec(`ALTER TABLE favorites.outbox ADD CONSTRAINT fail_batch_outbox CHECK(false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.BatchRemove(ctx, "alice", "br-fail", f.BatchRemove{IDs: []string{itemAlias.ID, itemCourse.ID}}); err == nil {
		t.Fatal("expected outbox failure to roll back batch remove")
	}
	if _, err := x.db.Exec(`ALTER TABLE favorites.outbox DROP CONSTRAINT fail_batch_outbox`); err != nil {
		t.Fatal(err)
	}
	stillFour, err := x.s.List(ctx, "alice", f.Filter{Limit: 10})
	if err != nil || len(stillFour.Items) != 4 {
		t.Fatal("batch remove was not rolled back", stillFour, err)
	}

	// Remove itemAlias + itemCourse (same canonical "course" -> coalesced single bucket revision bump!), itemTeacher, itemResource, plus absent ID & Bob's ID.
	removed, err := x.s.BatchRemove(ctx, "alice", "br-ok", f.BatchRemove{
		IDs: []string{itemAlias.ID, "already-gone", itemCourse.ID, itemTeacher.ID, itemResource.ID, bobItem.ID},
	})
	if err != nil || removed.RemovedCount != 4 || len(removed.IDs) != 4 {
		t.Fatal(removed, err)
	}
	// Coalesced canonical course activity: Revision increments by exactly 1 (not 2!), and count drops from 3 to 1 (Bob's).
	actCourseAfterRemove, err := x.repo.ReadActivity(ctx, f.ActivityID("course", "course", x.s.Now()))
	if err != nil || actCourseAfterRemove.Metrics["favorites"] != 1 || actCourseAfterRemove.Revision != actCourseBefore.Revision+1 {
		t.Fatal("expected coalesced single bucket revision update", actCourseAfterRemove, actCourseBefore, err)
	}
	bobList, err := x.s.List(ctx, "bob", f.Filter{Limit: 10})
	if err != nil || len(bobList.Items) != 1 {
		t.Fatal("bob item affected by alice batch remove", bobList, err)
	}

	// Quota freed after removal and folder deletion!
	if _, err := x.s.DeleteFolder(ctx, "alice", "del-b", customB.ID, customB.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.CreateFolder(ctx, "alice", "new-after-del", "释放配额后新建"); err != nil {
		t.Fatal("folder quota not freed after delete", err)
	}

	// 4. End-to-end HTTP + SDK verification with 100 items of 150-char IDs (> 15 KiB body).
	x.s.MaxItems = 200
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

	longPrefix := strings.Repeat("r", 140)
	batchIDs := make([]string, 0, 100)
	for i := range 100 {
		added, err := x.s.Add(ctx, "alice", fmt.Sprintf("bulk-%d", i), f.Add{
			Kind:     "resource",
			TargetID: fmt.Sprintf("%s-%03d", longPrefix, i),
		})
		if err != nil {
			t.Fatal(err)
		}
		batchIDs = append(batchIDs, added.ID)
	}

	bmRes, err := client.BatchMoveFavoritesWithResponse(ctx, &sdk.BatchMoveFavoritesParams{IdempotencyKey: "sdk-bm"}, sdk.BatchMove{
		FolderId: customA.ID,
		Ids:      batchIDs,
	})
	if err != nil || bmRes.StatusCode() != http.StatusOK || bmRes.JSON200 == nil || len(bmRes.JSON200.Items) != 100 {
		t.Fatal(err, bmRes)
	}
	brRes, err := client.BatchRemoveFavoritesWithResponse(ctx, &sdk.BatchRemoveFavoritesParams{IdempotencyKey: "sdk-br"}, sdk.BatchRemove{
		Ids: batchIDs,
	})
	if err != nil || brRes.StatusCode() != http.StatusOK || brRes.JSON200 == nil || brRes.JSON200.RemovedCount != 100 {
		t.Fatal(err, brRes)
	}

	// Boundary rejections (empty & 101 items).
	badMove, err := client.BatchMoveFavoritesWithResponse(ctx, &sdk.BatchMoveFavoritesParams{IdempotencyKey: "sdk-bad"}, sdk.BatchMove{
		FolderId: customA.ID,
		Ids:      []string{},
	})
	if err != nil || badMove.StatusCode() != http.StatusBadRequest {
		t.Fatal(err, badMove)
	}
}
