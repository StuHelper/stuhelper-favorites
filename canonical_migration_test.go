package favorites_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/StuHelper/stuhelper-favorites/postgres"
)

func TestCanonicalTargetDeduplicationAndLegacyMigration(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	x.s.Now = func() time.Time { return day }

	// A-02: Adding via alias ("old-alias" -> canonical "course") then via canonical ID ("course")
	// must return the existing favorite without inserting a second row or double-counting activity.
	first, err := x.s.Add(ctx, "alice", "add-alias", f.Add{Kind: "course", TargetID: "old-alias"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := x.s.Add(ctx, "alice", "add-canonical", f.Add{Kind: "course", TargetID: "course"})
	if err != nil {
		t.Fatalf("expected existing item on canonical duplicate, got err=%v", err)
	}
	if second.ID != first.ID || second.TargetID != "old-alias" {
		t.Fatalf("expected original receipt %+v, got %+v", first, second)
	}
	otherFolder, err := x.s.CreateFolder(ctx, "alice", "mk-folder", "备选")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.Add(ctx, "alice", "add-canonical-conflict", f.Add{Kind: "course", TargetID: "course", FolderID: otherFolder.ID}); !errors.Is(err, f.ErrConflict) {
		t.Fatalf("expected ErrConflict when canonical duplicate specifies different folder, got %v", err)
	}
	var itemCount int
	if err := x.db.QueryRow(`SELECT count(*) FROM favorites.items WHERE owner_key='alice'`).Scan(&itemCount); err != nil || itemCount != 1 {
		t.Fatalf("expected 1 item for alice, got %d (err=%v)", itemCount, err)
	}
	act, err := x.repo.ReadActivity(ctx, f.ActivityID("course", "course", day))
	if err != nil || act.Metrics["favorites"] != 1 {
		t.Fatalf("expected canonical activity favorites=1, got %+v (err=%v)", act, err)
	}

	// Database-level partial unique index must also reject direct duplicate canonical_target_id inserts.
	if _, err := x.db.Exec(`INSERT INTO favorites.items(owner_key,id,kind,target_id,canonical_target_id,title,folder_id,revision,created_at) VALUES('alice','manual-dup','course','another-alias','course','Title', $1, 1, now())`, first.FolderID); err == nil {
		t.Fatal("expected unique index violation on duplicate (owner_key, kind, canonical_target_id)")
	}

	// A-03 Part 1: Incremental Migrate on a legacy database whose favorites.items lacks canonical_target_id.
	dsn := os.Getenv("STUHELPER_FAVORITES_TEST_DSN")
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	legacyDBName := fmt.Sprintf("favorites_legacy_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + legacyDBName); err != nil {
		t.Fatal(err)
	}
	legacyOwner, err := sql.Open("pgx", dsn+" dbname="+legacyDBName)
	if err != nil {
		t.Fatal(err)
	}
	legacyRole := legacyDBName + "_runtime"
	t.Cleanup(func() {
		legacyOwner.Close()
		_, _ = admin.Exec(`DROP DATABASE ` + legacyDBName + ` WITH(FORCE)`)
		_, _ = admin.Exec(`DROP ROLE ` + legacyRole)
	})

	legacySchema := `
CREATE SCHEMA favorites;
CREATE TABLE favorites.owners(owner_key text PRIMARY KEY,closure_id text);
CREATE TABLE favorites.folders(owner_key text NOT NULL REFERENCES favorites.owners(owner_key),id text NOT NULL,name text NOT NULL CHECK(char_length(name)<=20),is_default boolean NOT NULL,revision bigint NOT NULL CHECK(revision>0),PRIMARY KEY(owner_key,id));
CREATE TABLE favorites.items(owner_key text NOT NULL,id text NOT NULL,kind text NOT NULL CHECK(kind IN('course','teacher','resource')),target_id text NOT NULL,title text NOT NULL CHECK(char_length(title)<=500),folder_id text NOT NULL,revision bigint NOT NULL CHECK(revision>0),created_at timestamptz NOT NULL,PRIMARY KEY(owner_key,id),UNIQUE(owner_key,kind,target_id),FOREIGN KEY(owner_key,folder_id) REFERENCES favorites.folders(owner_key,id));
INSERT INTO favorites.owners(owner_key) VALUES('legacy-a'),('legacy-b');
INSERT INTO favorites.folders(owner_key,id,name,is_default,revision) VALUES('legacy-a','f-a','',true,1),('legacy-b','f-b','',true,1);
INSERT INTO favorites.items(owner_key,id,kind,target_id,title,folder_id,revision,created_at) VALUES
  ('legacy-a','item-a','course','legacy-course-1','Legacy Course 1','f-a',1,'2026-11-01T00:00:00Z'),
  ('legacy-b','item-b','teacher','legacy-teacher-1','Legacy Teacher 1','f-b',1,'2026-11-01T00:00:00Z');
`
	if _, err := legacyOwner.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := postgres.Migrate(ctx, legacyOwner); err != nil {
			t.Fatalf("Migrate on legacy schema failed: %v", err)
		}
	}
	if _, err := legacyOwner.Exec(`CREATE ROLE ` + legacyRole + ` LOGIN;REVOKE ALL ON SCHEMA public FROM PUBLIC;GRANT USAGE ON SCHEMA favorites TO ` + legacyRole + `;GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA favorites TO ` + legacyRole + `;GRANT USAGE ON ALL SEQUENCES IN SCHEMA favorites TO ` + legacyRole + `;REVOKE UPDATE ON favorites.requests FROM ` + legacyRole + `;REVOKE UPDATE,DELETE ON favorites.events FROM ` + legacyRole); err != nil {
		t.Fatal(err)
	}
	legacyRuntime, err := sql.Open("pgx", dsn+" dbname="+legacyDBName+" user="+legacyRole)
	if err != nil {
		t.Fatal(err)
	}
	defer legacyRuntime.Close()
	legacyRepo := &postgres.Repository{DB: legacyRuntime}
	legacySvc := &f.Service{Repo: legacyRepo, Source: x.source, Now: func() time.Time { return day }}

	listed, err := legacySvc.List(ctx, "legacy-a", f.Filter{Limit: 10})
	if err != nil || len(listed.Items) != 1 || listed.Items[0].TargetID != "legacy-course-1" {
		t.Fatalf("List after legacy migration failed: %+v, %v", listed, err)
	}
	if _, err := legacySvc.Remove(ctx, "legacy-a", "rm-legacy", "item-a", 1); err != nil {
		t.Fatalf("Remove after legacy migration failed: %v", err)
	}
	if err := legacySvc.CloseOwner(ctx, "legacy-b", "44444444-4444-4444-8444-444444444444"); err != nil {
		t.Fatalf("CloseOwner after legacy migration failed: %v", err)
	}

	// A-03 Part 2: Defense-in-depth fallback when canonical_target_id is empty string.
	if _, err := legacyOwner.Exec(`ALTER TABLE favorites.items DROP CONSTRAINT IF EXISTS favorites_items_canonical_check`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacyOwner.Exec(`INSERT INTO favorites.owners(owner_key) VALUES('empty-c1'),('empty-c2'); INSERT INTO favorites.folders(owner_key,id,name,is_default,revision) VALUES('empty-c1','f1','',true,1),('empty-c2','f2','',true,1); INSERT INTO favorites.items(owner_key,id,kind,target_id,canonical_target_id,title,folder_id,revision,created_at) VALUES('empty-c1','i1','course','fallback-c1','','Fallback 1','f1',1,now()),('empty-c2','i2','course','fallback-c2','','Fallback 2','f2',1,now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacySvc.Remove(ctx, "empty-c1", "rm-empty", "i1", 1); err != nil {
		t.Fatalf("DeleteItem failed on empty canonical_target_id: %v", err)
	}
	if err := legacySvc.CloseOwner(ctx, "empty-c2", "55555555-5555-4555-8555-555555555555"); err != nil {
		t.Fatalf("CloseOwner failed on empty canonical_target_id: %v", err)
	}
}

func TestMigrationKeepsOldestCanonicalFavoritePerOwner(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	_, err := x.db.Exec(`DROP INDEX favorites.favorites_items_canonical;
 INSERT INTO favorites.owners(owner_key) VALUES('one'),('two');
 INSERT INTO favorites.folders(owner_key,id,name,is_default,revision)
 VALUES('one','a','',true,1),('one','b','other',false,1),('two','a','',true,1);
 INSERT INTO favorites.items(owner_key,id,kind,target_id,canonical_target_id,title,folder_id,revision,created_at)
 VALUES('one','old','course','alias','canonical','snapshot','a',3,'2024-01-01'),
 ('one','new','course','canonical','canonical','new','b',1,'2024-01-02'),
 ('two','other','course','canonical','canonical','snapshot','a',1,'2024-01-01')`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := postgres.Migrate(ctx, x.db); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		owner, id string
		rev       int64
	}{{"one", "old", 3}, {"two", "other", 1}} {
		items, err := x.s.List(ctx, tc.owner, f.Filter{Limit: 10})
		if err != nil || len(items.Items) != 1 || items.Items[0].ID != tc.id || items.Items[0].Revision != tc.rev || items.Items[0].FolderID != "a" {
			t.Fatal(tc, items, err)
		}
	}
}
