package favorites

import (
	"context"
	"encoding/base64"
	"strings"
	"time"
	_ "time/tzdata"
)

const ActivityEventType = "discovery.activity.v2"

var activityLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return location
}()

// ActivityTarget identifies the canonical catalog object receiving activity.
type ActivityTarget struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	ID     string `json:"id"`
}

// Activity is one complete provider-owned Beijing daily snapshot, including zeros.
type Activity struct {
	Source   string           `json:"source"`
	ID       string           `json:"id"`
	Target   ActivityTarget   `json:"target"`
	Day      string           `json:"day"`
	Revision int64            `json:"revision"`
	Visible  bool             `json:"visible"`
	Metrics  map[string]int64 `json:"metrics"`
}
type ActivityReader interface {
	// ActivitySnapshot receives Beijing midnight instants as inclusive start and exclusive end.
	ActivitySnapshot(context.Context, time.Time, time.Time, string, int) ([]Activity, string, error)
	ReadActivity(context.Context, string) (Activity, error)
}

// ActivityID identifies the canonical target and Beijing date for at.
func ActivityID(kind, id string, at time.Time) string {
	return kind + "/" + base64.RawURLEncoding.EncodeToString([]byte(id)) + "/" + ActivityDate(at)
}
func ParseActivityID(id string) (ActivityTarget, time.Time, error) {
	if len(id) == 0 || len(id) > 512 {
		return ActivityTarget{}, time.Time{}, ErrInput
	}
	parts := strings.Split(id, "/")
	if len(parts) != 3 || (parts[0] != "course" && parts[0] != "teacher") {
		return ActivityTarget{}, time.Time{}, ErrInput
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	target := string(raw)
	if e != nil || !valid(target, 200) {
		return ActivityTarget{}, time.Time{}, ErrInput
	}
	day, e := time.ParseInLocation(time.DateOnly, parts[2], activityLocation)
	if e != nil || id != ActivityID(parts[0], target, day) {
		return ActivityTarget{}, time.Time{}, ErrInput
	}
	return ActivityTarget{Source: "catalog", Kind: parts[0], ID: target}, day, nil
}

// ActivityDate returns the date label used in activity IDs and PostgreSQL DATE columns.
func ActivityDate(at time.Time) string {
	return at.In(activityLocation).Format(time.DateOnly)
}

// ActivityDayStart returns the Beijing midnight instant for the given time.
func ActivityDayStart(at time.Time) time.Time {
	y, m, d := at.In(activityLocation).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, activityLocation)
}

// ActivityDay reports whether t represents the start of a supported Beijing date.
func ActivityDay(t time.Time) bool {
	day := ActivityDayStart(t)
	return !t.IsZero() && day.Year() >= 1 && day.Year() <= 9999 && t.Equal(day)
}

// ActivityWindow reports whether the half-open calendar range contains 1 to 366 dates.
func ActivityWindow(from, until time.Time) bool {
	return ActivityDay(from) && ActivityDay(until) && from.Before(until) &&
		!until.After(ActivityDayStart(from).AddDate(0, 0, 366))
}
