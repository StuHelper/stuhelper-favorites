package favorites_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/httpapi"
	"github.com/StuHelper/stuhelper-favorites/sdk"
)

type dynamicSource struct {
	state atomic.Int32
	title atomic.Value
	calls atomic.Int32
}

func (d *dynamicSource) Resolve(ctx context.Context, owner, kind, id string) (f.ResolvedTarget, error) {
	d.calls.Add(1)
	if _, ok := ctx.Deadline(); !ok {
		return f.ResolvedTarget{}, errors.New("missing deadline")
	}
	switch d.state.Load() {
	case 1:
		return f.ResolvedTarget{}, f.ErrForbidden
	case 2:
		return f.ResolvedTarget{}, f.ErrNotFound
	case 3:
		return f.ResolvedTarget{}, f.ErrUnavailable
	}
	title, _ := d.title.Load().(string)
	if title == "" {
		title = "初始标题 " + id
	}
	return f.ResolvedTarget{ID: id, Kind: kind, Title: title}, nil
}

func TestTypedErrorReasonsAndTitleSnapshotRefresh(t *testing.T) {
	x := setup(t)
	dyn := &dynamicSource{}
	dyn.title.Store("高等数学A（旧版）")
	x.s.Source = dyn
	ctx := context.Background()

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
	if err != nil || foldersRes.StatusCode() != http.StatusOK || foldersRes.JSON200 == nil {
		t.Fatal(err, foldersRes)
	}
	defFolder := foldersRes.JSON200.Items[0]

	// 1. default_folder_protected (403)
	delDef, err := alice.DeleteFolderWithResponse(ctx, defFolder.Id, &sdk.DeleteFolderParams{IdempotencyKey: "del-def"}, sdk.Revision{Revision: 1})
	if err != nil || delDef.StatusCode() != http.StatusForbidden || delDef.JSON403 == nil || delDef.JSON403.Code != sdk.Forbidden || delDef.JSON403.Reason == nil || *delDef.JSON403.Reason != sdk.DefaultFolderProtected {
		t.Fatal("expected default_folder_protected", err, delDef)
	}

	// 2. folder_name_conflict (409) & idempotency_conflict (409) & revision_conflict (409)
	f1, err := alice.CreateFolderWithResponse(ctx, &sdk.CreateFolderParams{IdempotencyKey: "cf-1"}, sdk.Name{Name: "专业选修"})
	if err != nil || f1.StatusCode() != http.StatusOK || f1.JSON200 == nil {
		t.Fatal(err, f1)
	}
	dupFolder, err := alice.CreateFolderWithResponse(ctx, &sdk.CreateFolderParams{IdempotencyKey: "cf-dup"}, sdk.Name{Name: "专业选修"})
	if err != nil || dupFolder.StatusCode() != http.StatusConflict || dupFolder.JSON409 == nil || dupFolder.JSON409.Code != sdk.Conflict || dupFolder.JSON409.Reason == nil || *dupFolder.JSON409.Reason != sdk.FolderNameConflict {
		t.Fatal("expected folder_name_conflict", err, dupFolder)
	}
	idemConflict, err := alice.CreateFolderWithResponse(ctx, &sdk.CreateFolderParams{IdempotencyKey: "cf-1"}, sdk.Name{Name: "不同名称"})
	if err != nil || idemConflict.StatusCode() != http.StatusConflict || idemConflict.JSON409 == nil || idemConflict.JSON409.Reason == nil || *idemConflict.JSON409.Reason != sdk.IdempotencyConflict {
		t.Fatal("expected idempotency_conflict", err, idemConflict)
	}
	revConflict, err := alice.RenameFolderWithResponse(ctx, f1.JSON200.Id, &sdk.RenameFolderParams{IdempotencyKey: "rn-stale"}, sdk.Rename{Name: "新名称", Revision: 99})
	if err != nil || revConflict.StatusCode() != http.StatusConflict || revConflict.JSON409 == nil || revConflict.JSON409.Reason == nil || *revConflict.JSON409.Reason != sdk.RevisionConflict {
		t.Fatal("expected revision_conflict", err, revConflict)
	}

	// 3. folder_mismatch (409)
	added, err := alice.AddFavoriteWithResponse(ctx, &sdk.AddFavoriteParams{IdempotencyKey: "add-1"}, sdk.Add{
		Kind:     sdk.AddKindCourse,
		TargetId: "math101",
	})
	if err != nil || added.StatusCode() != http.StatusOK || added.JSON200 == nil {
		t.Fatal(err, added)
	}
	folderID := f1.JSON200.Id
	mismatch, err := alice.AddFavoriteWithResponse(ctx, &sdk.AddFavoriteParams{IdempotencyKey: "add-mismatch"}, sdk.Add{
		Kind:     sdk.AddKindCourse,
		TargetId: "math101",
		FolderId: &folderID,
	})
	if err != nil || mismatch.StatusCode() != http.StatusConflict || mismatch.JSON409 == nil || mismatch.JSON409.Reason == nil || *mismatch.JSON409.Reason != sdk.FolderMismatch {
		t.Fatal("expected folder_mismatch", err, mismatch)
	}

	// 4. Refresh title snapshot: unchanged title keeps revision=1; updated title increments revision=2 and updates search snapshot without touching activity.
	actBefore, err := x.repo.ReadActivity(ctx, f.ActivityID("course", "math101", x.s.Now()))
	if err != nil || actBefore.Metrics["favorites"] != 1 {
		t.Fatal(actBefore, err)
	}
	sameRefresh, err := alice.RefreshFavoriteWithResponse(ctx, added.JSON200.Id, &sdk.RefreshFavoriteParams{IdempotencyKey: "ref-same"})
	if err != nil || sameRefresh.StatusCode() != http.StatusOK || sameRefresh.JSON200 == nil || sameRefresh.JSON200.Revision != 1 || sameRefresh.JSON200.Title != "高等数学A（旧版）" {
		t.Fatal(err, sameRefresh)
	}

	dyn.title.Store("工科数学分析（2026新版）")
	updatedRefresh, err := alice.RefreshFavoriteWithResponse(ctx, added.JSON200.Id, &sdk.RefreshFavoriteParams{IdempotencyKey: "ref-updated"})
	if err != nil || updatedRefresh.StatusCode() != http.StatusOK || updatedRefresh.JSON200 == nil || updatedRefresh.JSON200.Revision != 2 || updatedRefresh.JSON200.Title != "工科数学分析（2026新版）" {
		t.Fatal(err, updatedRefresh)
	}
	actAfterRefresh, err := x.repo.ReadActivity(ctx, f.ActivityID("course", "math101", x.s.Now()))
	if err != nil || actAfterRefresh.Revision != actBefore.Revision || actAfterRefresh.Metrics["favorites"] != 1 {
		t.Fatal("refresh altered public activity", actAfterRefresh, err)
	}
	searched, err := x.s.List(ctx, "alice", f.Filter{Query: "2026新版", Limit: 10})
	if err != nil || len(searched.Items) != 1 || searched.Items[0].ID != added.JSON200.Id {
		t.Fatal("refreshed title not searchable", searched, err)
	}

	// Cross-owner refresh returns 404 without querying Source.Resolve.
	callsBeforeBob := dyn.calls.Load()
	bobRefresh, err := bob.RefreshFavoriteWithResponse(ctx, added.JSON200.Id, &sdk.RefreshFavoriteParams{IdempotencyKey: "bob-ref"})
	if err != nil || bobRefresh.StatusCode() != http.StatusNotFound || dyn.calls.Load() != callsBeforeBob {
		t.Fatal("cross-owner refresh leaked or queried upstream", err, bobRefresh)
	}

	// Target unpublished (404) or admission expired (403) preserves existing snapshot unchanged!
	for idx, st := range []int32{1, 2} {
		dyn.state.Store(st)
		kept, err := alice.RefreshFavoriteWithResponse(ctx, added.JSON200.Id, &sdk.RefreshFavoriteParams{IdempotencyKey: "ref-keep-" + string(rune('a'+idx))})
		if err != nil || kept.StatusCode() != http.StatusOK || kept.JSON200 == nil || kept.JSON200.Title != "工科数学分析（2026新版）" || kept.JSON200.Revision != 2 {
			t.Fatal("expected preserved snapshot when target hidden/forbidden", st, err, kept)
		}
	}

	// 5. admission_required (403) when adding a new target without admission.
	dyn.state.Store(1)
	deniedAdd, err := alice.AddFavoriteWithResponse(ctx, &sdk.AddFavoriteParams{IdempotencyKey: "add-denied"}, sdk.Add{
		Kind:     sdk.AddKindCourse,
		TargetId: "new-course",
	})
	if err != nil || deniedAdd.StatusCode() != http.StatusForbidden || deniedAdd.JSON403 == nil || deniedAdd.JSON403.Reason == nil || *deniedAdd.JSON403.Reason != sdk.AdmissionRequired {
		t.Fatal("expected admission_required", err, deniedAdd)
	}

	// Upstream outage (state=3) returns 503 and does not cache failure, allowing retry with same key.
	dyn.state.Store(3)
	outageRef, err := alice.RefreshFavoriteWithResponse(ctx, added.JSON200.Id, &sdk.RefreshFavoriteParams{IdempotencyKey: "ref-retry"})
	if err != nil || outageRef.StatusCode() != http.StatusServiceUnavailable {
		t.Fatal("expected 503 during upstream outage", err, outageRef)
	}
	dyn.state.Store(0)
	dyn.title.Store("工科数学分析（终版）")
	recoveredRef, err := alice.RefreshFavoriteWithResponse(ctx, added.JSON200.Id, &sdk.RefreshFavoriteParams{IdempotencyKey: "ref-retry"})
	if err != nil || recoveredRef.StatusCode() != http.StatusOK || recoveredRef.JSON200 == nil || recoveredRef.JSON200.Title != "工科数学分析（终版）" || recoveredRef.JSON200.Revision != 3 {
		t.Fatal("expected retry after 503 to succeed", err, recoveredRef)
	}

	// Unexpected JSON body on refresh is rejected with 400.
	badReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/favorites/items/"+added.JSON200.Id+"/refresh", strings.NewReader(`{"unexpected":true}`))
	badReq.Header.Set("Test-Session", "alice")
	badReq.Header.Set("Idempotency-Key", "ref-bad-body")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil || badResp.StatusCode != http.StatusBadRequest {
		t.Fatal("expected 400 for unexpected body", err, badResp)
	}
	badResp.Body.Close()

	// 6. owner_closed (403)
	if err := x.s.CloseOwner(ctx, "bob", "33333333-3333-4333-8333-333333333333"); err != nil {
		t.Fatal(err)
	}
	closedRes, err := bob.ListFoldersWithResponse(ctx, &sdk.ListFoldersParams{})
	if err != nil || closedRes.StatusCode() != http.StatusForbidden || closedRes.JSON403 == nil || closedRes.JSON403.Reason == nil || *closedRes.JSON403.Reason != sdk.OwnerClosed {
		t.Fatal("expected owner_closed", err, closedRes)
	}
}
