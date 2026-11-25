package favorites_test

import (
	"context"
	"errors"
	"testing"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
)

func TestUIDirectoryAllTimeDailyNet(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	day := f.ActivityDayStart(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC))
	now := day
	x.s.Now = func() time.Time { return now }
	item, err := x.s.Add(ctx, "owner", "add", f.Add{Kind: "course", TargetID: "course"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.AddDate(0, 0, 400)
	if _, err := x.s.Remove(ctx, "owner", "remove", item.ID, item.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.Add(ctx, "new-owner", "new", f.Add{Kind: "course", TargetID: "course"}); err != nil {
		t.Fatal(err)
	}
	rows, err := x.s.DirectoryActivitySnapshot(ctx, "course")
	if err != nil || len(rows) != 1 || rows[0].Count != 1 {
		t.Fatal("daily net or window changed", rows, err)
	}
	if _, err := x.s.DirectoryActivitySnapshot(ctx, "resource"); !errors.Is(err, f.ErrInput) {
		t.Fatal("invalid kind accepted", err)
	}
}

func TestActivityWindowAtBeijingMidnight(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	midnightUTC := time.Date(2024, 2, 29, 16, 0, 0, 0, time.UTC)
	now := midnightUTC.Add(-time.Nanosecond)
	x.s.Now = func() time.Time { return now }
	item, err := x.s.Add(ctx, "day-before", "add", f.Add{Kind: "course", TargetID: "course"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.ActivityDate(now); got != "2024-02-29" {
		t.Fatal(got)
	}
	now = midnightUTC
	if _, err := x.s.Remove(ctx, "day-before", "remove", item.ID, item.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.Add(ctx, "day-after", "add", f.Add{Kind: "course", TargetID: "course"}); err != nil {
		t.Fatal(err)
	}
	today := f.ActivityDayStart(now)
	if got := f.ActivityDate(today); got != "2024-03-01" || !today.Equal(midnightUTC) {
		t.Fatal(today, got)
	}
	rows, err := x.s.DirectoryActivitySnapshot(ctx, "course")
	if err != nil || len(rows) != 1 || rows[0].Count != 1 {
		t.Fatal("Beijing dates must be clamped independently", rows, err)
	}
}
