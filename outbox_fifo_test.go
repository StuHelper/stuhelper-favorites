package favorites_test

import (
	"context"
	"encoding/json"
	f "github.com/StuHelper/stuhelper-favorites"
	"testing"
)

func TestPendingOutboxDeliversOldestEligibleEvents(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	for i, kind := range []string{"teacher", "course", "course"} {
		owner := []string{"first", "second", "third"}[i]
		key := "add"
		if _, err := x.s.Add(ctx, owner, key, f.Add{Kind: kind, TargetID: kind}); err != nil {
			t.Fatal(err)
		}
	}
	teacher := f.ActivityID("teacher", "teacher", x.s.Now())
	course := f.ActivityID("course", "course", x.s.Now())
	assert := func(subject string, revision int64, excluded []string) f.Envelope {
		t.Helper()
		page, err := x.repo.PendingOutbox(ctx, 1, excluded)
		if err != nil || len(page) != 1 || page[0].Subject != subject {
			t.Fatalf("want %s, got %v, %v", subject, page, err)
		}
		var activity f.Activity
		if err := json.Unmarshal(page[0].Data, &activity); err != nil || activity.Revision != revision {
			t.Fatalf("want revision %d, got %d, err %v", revision, activity.Revision, err)
		}
		return page[0]
	}
	first := assert(teacher, 1, nil)
	second := assert(course, 1, []string{teacher})
	if err := x.repo.ConfirmOutbox(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if got := assert(course, 1, nil); got.ID != second.ID {
		t.Fatal("earliest course changed", got)
	}
	if err := x.repo.ConfirmOutbox(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if got := assert(course, 2, nil); got.ID == second.ID {
		t.Fatal("confirmed event still pending")
	}
}
