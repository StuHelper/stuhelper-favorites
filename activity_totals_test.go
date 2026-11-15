package favorites_test

import (
	"context"
	"errors"
	"testing"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
)

func TestUIDirectoryActivityTotalsRespectWindowAndDailyNet(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	day := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	now := day
	x.s.Now = func() time.Time { return now }
	item, err := x.s.Add(ctx, "owner", "add", f.Add{Kind: "course", TargetID: "course"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	if _, err := x.s.Remove(ctx, "owner", "remove", item.ID, item.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.Add(ctx, "new-owner", "new", f.Add{Kind: "course", TargetID: "course"}); err != nil {
		t.Fatal(err)
	}
	rows, err := x.s.DirectoryActivitySnapshot(ctx, "course", day, day.Add(48*time.Hour))
	if err != nil || len(rows) != 1 || rows[0].Count != 1 {
		t.Fatal("daily net or window changed", rows, err)
	}
	rows, err = x.s.DirectoryActivitySnapshot(ctx, "course", day.Add(24*time.Hour), day.Add(48*time.Hour))
	if err != nil || len(rows) != 0 {
		t.Fatal("zero bucket counted", rows, err)
	}
	for _, test := range []struct {
		kind     string
		from, to time.Time
	}{{"resource", day, day.Add(24 * time.Hour)}, {"course", day, day}, {"course", day.Add(time.Hour), day.Add(24 * time.Hour)}} {
		if _, err := x.s.DirectoryActivitySnapshot(ctx, test.kind, test.from, test.to); !errors.Is(err, f.ErrInput) {
			t.Fatal("invalid window accepted", err)
		}
	}
}
