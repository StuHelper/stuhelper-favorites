package favorites_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/postgres"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"os"
)

type customTargetSource struct {
	canonical string
}

func (c customTargetSource) Resolve(_ context.Context, _, kind, id string) (f.ResolvedTarget, error) {
	return f.ResolvedTarget{ID: c.canonical, Kind: kind, Title: "Title " + id}, nil
}

func TestUnicodeActivityIDCursorValidationAndSanitizedDatabaseErrors(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	day := f.ActivityDayStart(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	x.s.Now = func() time.Time { return day }

	// A-05 Part 1: 70-rune CJK canonical target ID (210 UTF-8 bytes > 200, ActivityID len 298 <= 512)
	// must be accepted by ParseActivityID, Service.Add, and discovery-activity.schema.json.
	cjk70 := strings.Repeat("课", 70)
	actID := f.ActivityID("course", cjk70, day)
	parsed, parsedDay, err := f.ParseActivityID(actID)
	if err != nil || parsed.ID != cjk70 || !parsedDay.Equal(day) {
		t.Fatalf("ParseActivityID rejected valid 70-rune CJK target: parsed=%+v day=%v err=%v", parsed, parsedDay, err)
	}
	svcCJK := &f.Service{Repo: x.repo, Source: customTargetSource{canonical: cjk70}, Now: func() time.Time { return day }}
	if _, err := svcCJK.Add(ctx, "alice", "cjk-70", f.Add{Kind: "course", TargetID: "short-alias"}); err != nil {
		t.Fatalf("Add failed when canonical target has 70 CJK runes: %v", err)
	}
	rawSchema, err := os.ReadFile("contracts/discovery-activity.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var def any
	if err := json.Unmarshal(rawSchema, &def); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("urn:activity", def); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("urn:activity")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := x.repo.PendingOutbox(ctx, 10, nil)
	if err != nil || len(pending) == 0 {
		t.Fatalf("expected pending outbox event, got %v, %v", pending, err)
	}
	for _, ev := range pending {
		b, _ := json.Marshal(ev)
		var v any
		_ = json.Unmarshal(b, &v)
		if err := schema.Validate(v); err != nil {
			t.Fatalf("70-rune CJK outbox event failed schema validation: %v", err)
		}
	}

	// Control characters (e.g. NUL byte "AA" -> "\x00") and >512-byte ActivityID must be rejected by ParseActivityID.
	if _, _, err := f.ParseActivityID("course/AA/2026-11-09"); !errors.Is(err, f.ErrInput) {
		t.Fatalf("expected ErrInput for NUL byte in ActivityID, got %v", err)
	}
	if _, _, err := x.s.CurrentActivity(ctx, "alice", "course/AA/2026-11-09"); !errors.Is(err, f.ErrInput) {
		t.Fatalf("expected ErrInput from CurrentActivity for NUL ActivityID, got %v", err)
	}
	// 150 CJK runes = 450 UTF-8 bytes -> base64 600 chars -> ActivityID > 512 chars: upstream canonical ID must yield ErrUnavailable (503), not ErrInput (400).
	cjk150 := strings.Repeat("课", 150)
	if _, _, err := f.ParseActivityID(f.ActivityID("course", cjk150, day)); !errors.Is(err, f.ErrInput) {
		t.Fatalf("expected ErrInput for ActivityID exceeding 512 chars, got %v", err)
	}
	svcTooLong := &f.Service{Repo: x.repo, Source: customTargetSource{canonical: cjk150}, Now: func() time.Time { return day }}
	if _, err := svcTooLong.Add(ctx, "alice", "cjk-150", f.Add{Kind: "course", TargetID: "alias-150"}); !errors.Is(err, f.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable when upstream canonical ID exceeds 512-char ActivityID limit, got %v", err)
	}

	// A-10 Part 1: Tampered List cursor (NUL in Title or zero CreatedAt) and tampered ActivitySnapshot cursor must return ErrInput (400), not ErrUnavailable (503).
	if _, err := x.s.Add(ctx, "alice", "item-2", f.Add{Kind: "teacher", TargetID: "t-1"}); err != nil {
		t.Fatal(err)
	}
	page1, err := x.s.List(ctx, "alice", f.Filter{Limit: 1})
	if err != nil || page1.Next == "" {
		t.Fatalf("expected paginated list, got %+v, %v", page1, err)
	}
	rawPos, err := base64.RawURLEncoding.DecodeString(page1.Next)
	if err != nil {
		t.Fatal(err)
	}
	var pos f.Position
	if err := json.Unmarshal(rawPos, &pos); err != nil {
		t.Fatal(err)
	}
	nulPos := pos
	nulPos.Title = "bad\x00title"
	nulBytes, _ := json.Marshal(nulPos)
	if _, err := x.s.List(ctx, "alice", f.Filter{Limit: 1, After: base64.RawURLEncoding.EncodeToString(nulBytes)}); !errors.Is(err, f.ErrInput) {
		t.Fatalf("expected ErrInput for NUL byte in cursor Title, got %v", err)
	}
	zeroTimePos := pos
	zeroTimePos.CreatedAt = time.Time{}
	zeroBytes, _ := json.Marshal(zeroTimePos)
	if _, err := x.s.List(ctx, "alice", f.Filter{Limit: 1, After: base64.RawURLEncoding.EncodeToString(zeroBytes)}); !errors.Is(err, f.ErrInput) {
		t.Fatalf("expected ErrInput for zero CreatedAt in cursor, got %v", err)
	}

	_, snapCursor, err := x.repo.ActivitySnapshot(ctx, day, day.AddDate(0, 0, 1), "", 1)
	if err != nil || snapCursor == "" {
		t.Fatalf("expected non-empty ActivitySnapshot cursor, got %q, %v", snapCursor, err)
	}
	snapRaw, _ := base64.RawURLEncoding.DecodeString(snapCursor)
	var snapMap map[string]any
	_ = json.Unmarshal(snapRaw, &snapMap)
	snapMap["Visibility"] = "not-a-pg-snapshot"
	badSnapBytes, _ := json.Marshal(snapMap)
	if _, _, err := x.repo.ActivitySnapshot(ctx, day, day.AddDate(0, 0, 1), base64.RawURLEncoding.EncodeToString(badSnapBytes), 1); !errors.Is(err, f.ErrInput) {
		t.Fatalf("expected ErrInput for malformed pg_snapshot cursor, got %v", err)
	}
	snapMap["Visibility"] = "10:10:"
	snapMap["After"] = "course/AA/2026-11-09"
	badAfterBytes, _ := json.Marshal(snapMap)
	if _, _, err := x.repo.ActivitySnapshot(ctx, day, day.AddDate(0, 0, 1), base64.RawURLEncoding.EncodeToString(badAfterBytes), 1); !errors.Is(err, f.ErrInput) {
		t.Fatalf("expected ErrInput for malformed After ActivityID in cursor, got %v", err)
	}

	// CloseOwner input validation (both Service and Repository).
	if err := x.s.CloseOwner(ctx, "alice", strings.Repeat("\x00", 36)); !errors.Is(err, f.ErrInput) {
		t.Fatalf("expected ErrInput for control chars in Service.CloseOwner id, got %v", err)
	}
	if err := x.repo.CloseOwner(ctx, "", "valid-id"); !errors.Is(err, f.ErrInput) {
		t.Fatalf("expected ErrInput for empty owner in Repository.CloseOwner, got %v", err)
	}
	if err := x.repo.CloseOwner(ctx, "alice", "bad\x00id"); !errors.Is(err, f.ErrInput) {
		t.Fatalf("expected ErrInput for NUL id in Repository.CloseOwner, got %v", err)
	}

	// A-10 Part 2: Database failures preserve sanitized Op + SQLState while still matching errors.Is(..., ErrUnavailable),
	// and transient deadlock/serialization (40P01 / 40001) map to ErrUnavailable rather than business 409 ErrConflict.
	if _, err := x.db.Exec(`CREATE FUNCTION favorites.raise_deadlock() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'simulated deadlock' USING ERRCODE = '40P01'; END $$; CREATE TRIGGER raise_deadlock BEFORE INSERT ON favorites.events FOR EACH ROW WHEN(NEW.operation='deadlock-op') EXECUTE FUNCTION favorites.raise_deadlock()`); err != nil {
		t.Fatal(err)
	}
	_, dlErr := x.s.CreateFolder(ctx, "alice", "deadlock-op", "DeadlockFolder")
	if !errors.Is(dlErr, f.ErrUnavailable) || errors.Is(dlErr, f.ErrConflict) {
		t.Fatalf("expected 40P01 deadlock to map to ErrUnavailable (not ErrConflict), got %v", dlErr)
	}
	var dbDiag *postgres.Error
	if !errors.As(dlErr, &dbDiag) || dbDiag.SQLState != "40P01" || dbDiag.Op == "" {
		t.Fatalf("expected sanitized *postgres.Error with SQLState=40P01 and non-empty Op, got %T (%v)", dlErr, dlErr)
	}
	if strings.Contains(dlErr.Error(), "alice") || strings.Contains(dlErr.Error(), "DeadlockFolder") || strings.Contains(dlErr.Error(), "simulated deadlock") {
		t.Fatalf("sanitized error leaked raw message or user payload: %s", dlErr.Error())
	}
}
