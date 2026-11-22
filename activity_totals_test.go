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
	day := f.ActivityDayStart(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC))
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
	before := f.ActivityDayStart(now)
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
	after := today.AddDate(0, 0, 1)
	for _, tc := range []struct {
		name  string
		start time.Time
		end   time.Time
		count int
	}{
		{"leap day only", before, today, 1},
		{"March only", today, after, 0},
		{"366 dates including March", after.AddDate(0, 0, -366), after, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := x.s.DirectoryActivitySnapshot(ctx, "course", tc.start.UTC(), tc.end.UTC())
			if err != nil || len(rows) != tc.count || len(rows) > 0 && rows[0].Count != 1 {
				t.Fatal(rows, err)
			}
		})
	}
	if _, err := x.s.DirectoryActivitySnapshot(ctx, "course", after.AddDate(0, 0, -367), after); !errors.Is(err, f.ErrInput) {
		t.Fatal(err)
	}
}
