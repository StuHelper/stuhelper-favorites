package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	f "github.com/StuHelper/stuhelper-favorites"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

//go:embed schema.sql
var Schema string

// Error preserves a sanitized repository operation name and PostgreSQL SQLSTATE
// for server-side diagnostics while unwrapping to favorites.ErrUnavailable.
type Error struct {
	Op       string
	SQLState string
}

func (e *Error) Error() string {
	if e == nil {
		return f.ErrUnavailable.Error()
	}
	if e.SQLState != "" {
		return "favorites postgres " + e.Op + ": unavailable (sqlstate " + e.SQLState + ")"
	}
	if e.Op != "" {
		return "favorites postgres " + e.Op + ": unavailable"
	}
	return f.ErrUnavailable.Error()
}
func (e *Error) Unwrap() error { return f.ErrUnavailable }

type Repository struct{ DB *sql.DB }

func (r *Repository) readyDB() bool { return r != nil && r.DB != nil }

type transaction struct {
	ctx   context.Context
	tx    *sql.Tx
	owner string
}

func validText(s string, max int) bool {
	return s != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsFunc(s, unicode.IsControl)
}
func safeOp(op string, e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, sql.ErrNoRows) {
		return f.ErrNotFound
	}
	if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return e
	}
	var diag *Error
	if errors.As(e, &diag) {
		return e
	}
	var p *pgconn.PgError
	if errors.As(e, &p) {
		if p.Code == "23505" || p.Code == "23503" {
			return f.ErrConflict
		}
		return &Error{Op: op, SQLState: p.Code}
	}
	return &Error{Op: op}
}
func safe(e error) error { return safeOp("db", e) }
func Migrate(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return f.ErrUnavailable
	}
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(726391840119)`); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, Schema); e != nil {
		return e
	}
	if e = switchActivityCalendar(ctx, tx, true); e != nil {
		return e
	}
	return tx.Commit()
}
func (r *Repository) Ready(ctx context.Context) error {
	if !r.readyDB() {
		return f.ErrUnavailable
	}
	var ready bool
	e := r.DB.QueryRowContext(ctx, `SELECT
   has_schema_privilege(current_user,'favorites','USAGE')
   AND EXISTS(SELECT 1 FROM favorites.activity_retention WHERE singleton AND shanghai_day)
   AND NOT EXISTS(
      SELECT 1 FROM (VALUES
				('favorites.owners',ARRAY['SELECT','INSERT','UPDATE']),
				('favorites.folders',ARRAY['SELECT','INSERT','UPDATE','DELETE']),
				('favorites.items',ARRAY['SELECT','INSERT','UPDATE','DELETE']),
				('favorites.requests',ARRAY['SELECT','INSERT','DELETE']),
				('favorites.events',ARRAY['SELECT','INSERT']),
				('favorites.activity_buckets',ARRAY['SELECT','INSERT','UPDATE']),
				('favorites.activity_history',ARRAY['SELECT','INSERT']),
				('favorites.activity_retention',ARRAY['SELECT']),
				('favorites.outbox',ARRAY['SELECT','INSERT','UPDATE'])
			) AS required(tbl,privs),
			LATERAL unnest(required.privs) AS priv
			WHERE NOT has_table_privilege(current_user,required.tbl,priv)
		)
		AND has_sequence_privilege(current_user,pg_get_serial_sequence('favorites.events','id'),'USAGE')
   AND has_sequence_privilege(current_user,pg_get_serial_sequence('favorites.outbox','seq'),'USAGE')`).Scan(&ready)
	if e != nil || !ready {
		return f.ErrUnavailable
	}
	return nil
}

func (r *Repository) CheckRuntime(ctx context.Context) error {
	if e := r.Ready(ctx); e != nil {
		return e
	}
	var unsafe bool
	e := r.DB.QueryRowContext(ctx, `SELECT rolsuper OR rolcreaterole OR rolcreatedb
    OR has_schema_privilege(current_user,'favorites','CREATE')
    OR has_table_privilege(current_user,'favorites.events','UPDATE,DELETE')
    OR has_table_privilege(current_user,'favorites.requests','UPDATE')
    OR has_table_privilege(current_user,'favorites.owners','DELETE')
    OR has_table_privilege(current_user,'favorites.activity_history','UPDATE,DELETE')
    OR has_table_privilege(current_user,'favorites.activity_retention','INSERT,UPDATE,DELETE')
    OR has_table_privilege(current_user,'favorites.activity_buckets','DELETE')
    OR has_table_privilege(current_user,'favorites.outbox','DELETE')
    OR EXISTS(SELECT 1 FROM pg_tables WHERE schemaname='favorites'
       AND has_table_privilege(current_user,format('%I.%I',schemaname,tablename),'TRUNCATE'))
    FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe)
	if e != nil || unsafe {
		return f.ErrUnavailable
	}
	return nil
}
func (r *Repository) Transaction(ctx context.Context, owner string, run func(f.Transaction) error) error {
	if !r.readyDB() {
		return f.ErrUnavailable
	}
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return safeOp("tx.begin", e)
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "favorites:"+owner); e != nil {
		return safeOp("tx.lock", e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO favorites.owners(owner_key) VALUES($1) ON CONFLICT DO NOTHING`, owner); e != nil {
		return safeOp("owners.ensure", e)
	}
	var closed bool
	if e = tx.QueryRowContext(ctx, `SELECT closure_id IS NOT NULL FROM favorites.owners WHERE owner_key=$1`, owner).Scan(&closed); e != nil {
		return safeOp("owners.status", e)
	}
	if closed {
		return f.WithReason(f.ErrForbidden, f.ReasonOwnerClosed)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO favorites.folders(owner_key,id,name,is_default,revision) SELECT $1,gen_random_uuid()::text,'',true,1 WHERE NOT EXISTS(SELECT 1 FROM favorites.folders WHERE owner_key=$1 AND is_default)`, owner); e != nil {
		return safeOp("folders.ensure_default", e)
	}
	if e = run(&transaction{ctx, tx, owner}); e != nil {
		return e
	}
	return safeOp("tx.commit", tx.Commit())
}
func (t *transaction) Replay(key string) (string, json.RawMessage, error) {
	var h string
	var b []byte
	var expired bool
	e := t.tx.QueryRowContext(t.ctx, `SELECT digest,result,created_at <= now()-interval '24 hours' FROM favorites.requests WHERE owner_key=$1 AND key=$2`, t.owner, key).Scan(&h, &b, &expired)
	if e == nil && expired {
		// Writers for this owner already hold the advisory lock. The cleaner may have removed the receipt.
		if _, err := t.tx.ExecContext(t.ctx, `DELETE FROM favorites.requests WHERE owner_key=$1 AND key=$2`, t.owner, key); err != nil {
			return "", nil, safeOp("requests.expire", err)
		}
		return "", nil, f.ErrNotFound
	}
	return h, b, safeOp("requests.replay", e)
}
func (t *transaction) SaveReplay(key, h string, b json.RawMessage) error {
	_, e := t.tx.ExecContext(t.ctx, `INSERT INTO favorites.requests(owner_key,key,digest,result) VALUES($1,$2,$3,$4)`, t.owner, key, h, string(b))
	return safeOp("requests.save", e)
}
func (t *transaction) Event(kind, key string) error {
	_, e := t.tx.ExecContext(t.ctx, `INSERT INTO favorites.events(owner_key,kind,operation) VALUES($1,$2,$3)`, t.owner, kind, key)
	return safeOp("events.insert", e)
}

type scanner interface{ Scan(...any) error }

func folder(row scanner) (f.Folder, error) {
	var v f.Folder
	e := row.Scan(&v.ID, &v.Name, &v.Default, &v.Revision, &v.Position, &v.CreatedAt, &v.ItemCount, &v.KindCounts.Course, &v.KindCounts.Teacher, &v.KindCounts.Resource)
	return v, safe(e)
}
func (t *transaction) Folder(id string) (f.Folder, error) {
	return folder(t.tx.QueryRowContext(t.ctx, `WITH counts AS (SELECT count(*) AS total,count(*) FILTER(WHERE kind='course') AS course_count,count(*) FILTER(WHERE kind='teacher') AS teacher_count,count(*) FILTER(WHERE kind='resource') AS resource_count FROM favorites.items WHERE owner_key=$1 AND folder_id=$2) SELECT f.id,f.name,f.is_default,f.revision,f.position,f.created_at,c.total,c.course_count,c.teacher_count,c.resource_count FROM favorites.folders f CROSS JOIN counts c WHERE f.owner_key=$1 AND f.id=$2`, t.owner, id))
}
func (t *transaction) FolderCount() (int, error) {
	var n int
	e := t.tx.QueryRowContext(t.ctx, `SELECT count(*) FROM favorites.folders WHERE owner_key=$1`, t.owner).Scan(&n)
	return n, safe(e)
}
func (t *transaction) DefaultFolder() (f.Folder, error) {
	return folder(t.tx.QueryRowContext(t.ctx, `WITH target AS (SELECT id,name,is_default,revision,position,created_at FROM favorites.folders WHERE owner_key=$1 AND is_default),counts AS (SELECT count(*) AS total,count(*) FILTER(WHERE kind='course') AS course_count,count(*) FILTER(WHERE kind='teacher') AS teacher_count,count(*) FILTER(WHERE kind='resource') AS resource_count FROM favorites.items WHERE owner_key=$1 AND folder_id=(SELECT id FROM target)) SELECT t.id,t.name,t.is_default,t.revision,t.position,t.created_at,c.total,c.course_count,c.teacher_count,c.resource_count FROM target t CROSS JOIN counts c`, t.owner))
}
func (t *transaction) Folders(pos f.FolderPosition, limit int) ([]f.Folder, error) {
	rows, e := t.tx.QueryContext(t.ctx, `WITH counts AS (SELECT folder_id,count(*) AS total,count(*) FILTER(WHERE kind='course') AS course_count,count(*) FILTER(WHERE kind='teacher') AS teacher_count,count(*) FILTER(WHERE kind='resource') AS resource_count FROM favorites.items WHERE owner_key=$1 GROUP BY folder_id) SELECT f.id,f.name,f.is_default,f.revision,f.position,f.created_at,COALESCE(c.total,0),COALESCE(c.course_count,0),COALESCE(c.teacher_count,0),COALESCE(c.resource_count,0) FROM favorites.folders f LEFT JOIN counts c ON c.folder_id=f.id WHERE f.owner_key=$1 AND ($2='' OR (NOT f.is_default,f.position,f.created_at,f.id COLLATE "C")>(NOT $3,$4,$5::timestamptz,$2 COLLATE "C")) ORDER BY f.is_default DESC,f.position ASC,f.created_at ASC,f.id COLLATE "C" ASC LIMIT $6`, t.owner, pos.ID, pos.Default, pos.Position, pos.CreatedAt, limit)
	if e != nil {
		return nil, safe(e)
	}
	defer rows.Close()
	out := []f.Folder{}
	for rows.Next() {
		v, e := folder(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, safe(rows.Err())
}
func (t *transaction) SaveFolder(v f.Folder) error {
	createdAt := v.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, e := t.tx.ExecContext(t.ctx, `INSERT INTO favorites.folders(owner_key,id,name,is_default,revision,position,created_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(owner_key,id) DO UPDATE SET name=EXCLUDED.name,revision=EXCLUDED.revision`, t.owner, v.ID, v.Name, v.Default, v.Revision, v.Position, createdAt)
	var p *pgconn.PgError
	if errors.As(e, &p) && p.Code == "23505" && p.ConstraintName == "favorites_folder_names" {
		return f.WithReason(f.ErrConflict, f.ReasonFolderNameConflict)
	}
	return safeOp("folders.save", e)
}
func (t *transaction) DeleteFolder(id, defaultID string) error {
	if _, e := t.tx.ExecContext(t.ctx, `UPDATE favorites.items SET folder_id=$3,revision=revision+1 WHERE owner_key=$1 AND folder_id=$2`, t.owner, id, defaultID); e != nil {
		return safe(e)
	}
	_, e := t.tx.ExecContext(t.ctx, `DELETE FROM favorites.folders WHERE owner_key=$1 AND id=$2 AND NOT is_default`, t.owner, id)
	return safe(e)
}

const columns = `id,kind,target_id,title,folder_id,revision,created_at,canonical_target_id`

func item(row scanner) (f.Item, error) {
	var v f.Item
	e := row.Scan(&v.ID, &v.Kind, &v.TargetID, &v.Title, &v.FolderID, &v.Revision, &v.CreatedAt, &v.CanonicalTargetID)
	return v, safe(e)
}
func (t *transaction) Item(id string) (f.Item, error) {
	return item(t.tx.QueryRowContext(t.ctx, `SELECT `+columns+` FROM favorites.items WHERE owner_key=$1 AND id=$2`, t.owner, id))
}
func (t *transaction) ItemCount() (int, error) {
	var n int
	e := t.tx.QueryRowContext(t.ctx, `SELECT count(*) FROM favorites.items WHERE owner_key=$1`, t.owner).Scan(&n)
	return n, safe(e)
}
func (t *transaction) Target(kind, target string) (f.Item, error) {
	return item(t.tx.QueryRowContext(t.ctx, `SELECT `+columns+` FROM favorites.items WHERE owner_key=$1 AND kind=$2 AND target_id=$3`, t.owner, kind, target))
}
func (t *transaction) CanonicalTarget(kind, canonical string) (f.Item, error) {
	return item(t.tx.QueryRowContext(t.ctx, `SELECT `+columns+` FROM favorites.items WHERE owner_key=$1 AND kind=$2 AND canonical_target_id=$3`, t.owner, kind, canonical))
}
func (t *transaction) Items(filter f.Filter, pos f.Position) ([]f.Item, error) {
	order := `created_at DESC,id`
	page := `($5='' OR created_at<$6 OR (created_at=$6 AND id>$5))`
	if filter.Sort == "name" {
		order = `title COLLATE "C",id`
		page = `($5='' OR title COLLATE "C">$7 COLLATE "C" OR (title=$7 AND id>$5))`
	}
	rows, e := t.tx.QueryContext(t.ctx, `SELECT `+columns+` FROM favorites.items WHERE owner_key=$1 AND ($2='' OR folder_id=$2) AND ($3='' OR kind=$3) AND ($4='' OR strpos(lower(title),lower($4))>0) AND `+page+` AND $6::timestamptz IS NOT NULL AND $7::text IS NOT NULL AND ($9::text[] IS NULL OR target_id=ANY($9::text[])) ORDER BY `+order+` LIMIT $8`, t.owner, filter.FolderID, filter.Kind, filter.Query, pos.ID, pos.CreatedAt, pos.Title, filter.Limit, filter.TargetIDs)
	if e != nil {
		return nil, safe(e)
	}
	defer rows.Close()
	out := []f.Item{}
	for rows.Next() {
		v, e := item(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, safe(rows.Err())
}
func (t *transaction) SaveItem(v f.Item) error {
	canonical := v.CanonicalTargetID
	if canonical == "" {
		canonical = v.TargetID
	}
	var exists bool
	if e := t.tx.QueryRowContext(t.ctx, `SELECT EXISTS(SELECT 1 FROM favorites.items WHERE owner_key=$1 AND id=$2)`, t.owner, v.ID).Scan(&exists); e != nil {
		return safe(e)
	}
	_, e := t.tx.ExecContext(t.ctx, `INSERT INTO favorites.items(owner_key,`+columns+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(owner_key,id) DO UPDATE SET title=EXCLUDED.title,folder_id=EXCLUDED.folder_id,revision=EXCLUDED.revision`, t.owner, v.ID, v.Kind, v.TargetID, v.Title, v.FolderID, v.Revision, v.CreatedAt, canonical)
	if e != nil {
		return safe(e)
	}
	if !exists && (v.Kind == "course" || v.Kind == "teacher") {
		return t.changeActivity(v.Kind, canonical, 1, v.CreatedAt)
	}
	return nil
}
func (t *transaction) DeleteItem(id string, at time.Time) error {
	var kind, target string
	e := t.tx.QueryRowContext(t.ctx, `DELETE FROM favorites.items WHERE owner_key=$1 AND id=$2 RETURNING kind,COALESCE(NULLIF(canonical_target_id,''),target_id)`, t.owner, id).Scan(&kind, &target)
	if e != nil {
		return safe(e)
	}
	if kind == "course" || kind == "teacher" {
		return t.changeActivity(kind, target, -1, at)
	}
	return nil
}
func (t *transaction) MoveItems(ids []string, folderID string) ([]f.Item, error) {
	rows, e := t.tx.QueryContext(t.ctx, `UPDATE favorites.items SET folder_id=$3,revision=CASE WHEN folder_id=$3 THEN revision ELSE revision+1 END WHERE owner_key=$1 AND id=ANY($2::text[]) RETURNING `+columns, t.owner, ids, folderID)
	if e != nil {
		return nil, safe(e)
	}
	byID := make(map[string]f.Item, len(ids))
	for rows.Next() {
		v, err := item(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		byID[v.ID] = v
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, safe(e)
	}
	if len(byID) != len(ids) {
		return nil, f.ErrNotFound
	}
	out := make([]f.Item, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out, nil
}
func (t *transaction) DeleteItems(ids []string, at time.Time) ([]string, error) {
	rows, e := t.tx.QueryContext(t.ctx, `DELETE FROM favorites.items WHERE owner_key=$1 AND id=ANY($2::text[]) RETURNING id,kind,COALESCE(NULLIF(canonical_target_id,''),target_id)`, t.owner, ids)
	if e != nil {
		return nil, safe(e)
	}
	type bucketKey struct{ kind, target string }
	counts := map[bucketKey]int64{}
	deleted := make(map[string]struct{}, len(ids))
	for rows.Next() {
		var id, kind, target string
		if err := rows.Scan(&id, &kind, &target); err != nil {
			rows.Close()
			return nil, safe(err)
		}
		deleted[id] = struct{}{}
		if kind == "course" || kind == "teacher" {
			counts[bucketKey{kind, target}]++
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, safe(e)
	}
	type bucketDelta struct {
		activityID   string
		kind, target string
		delta        int64
	}
	deltas := make([]bucketDelta, 0, len(counts))
	for k, n := range counts {
		deltas = append(deltas, bucketDelta{activityID: f.ActivityID(k.kind, k.target, at), kind: k.kind, target: k.target, delta: -n})
	}
	slices.SortFunc(deltas, func(a, b bucketDelta) int { return strings.Compare(a.activityID, b.activityID) })
	for _, d := range deltas {
		if err := t.changeActivity(d.kind, d.target, d.delta, at); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(deleted))
	for _, id := range ids {
		if _, ok := deleted[id]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}
func (r *Repository) CourseReferences(ctx context.Context, owner string, ids []string) ([]string, error) {
	if !r.readyDB() {
		return nil, f.ErrUnavailable
	}
	rows, e := r.DB.QueryContext(ctx, `SELECT DISTINCT target_id FROM favorites.items WHERE owner_key=$1 AND kind='course' AND target_id=ANY($2::text[]) ORDER BY target_id`, owner, ids)
	if e != nil {
		return nil, safe(e)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, safe(e)
		}
		out = append(out, id)
	}
	return out, safe(rows.Err())
}
func (r *Repository) NeedsTargetResolution(ctx context.Context, owner, key string, in f.Add) (bool, error) {
	var closed, known, folderOK bool
	err := r.DB.QueryRowContext(ctx, `SELECT
   EXISTS(SELECT FROM favorites.owners WHERE owner_key=$1 AND closure_id IS NOT NULL),
   EXISTS(SELECT FROM favorites.requests WHERE owner_key=$1 AND key=$2 AND created_at > now()-interval '24 hours')
     OR EXISTS(SELECT FROM favorites.items WHERE owner_key=$1 AND kind=$3 AND target_id=$4),
   $5='' OR EXISTS(SELECT FROM favorites.folders WHERE owner_key=$1 AND id=$5)`,
		owner, key, in.Kind, in.TargetID, in.FolderID).Scan(&closed, &known, &folderOK)
	if err != nil {
		return false, safe(err)
	}
	if closed {
		return false, f.WithReason(f.ErrForbidden, f.ReasonOwnerClosed)
	}
	if known {
		return false, nil
	}
	if !folderOK {
		return false, f.ErrNotFound
	}
	return true, nil
}

// ReadFolderPage checks closure and reads the ordered page from one read-only snapshot.
func (r *Repository) ReadFolderPage(ctx context.Context, owner string, pos f.FolderPosition, limit int) ([]f.Folder, bool, error) {
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, false, safeOp("folders.begin_read", err)
	}
	defer tx.Rollback()
	var closed, initialized bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM favorites.owners WHERE owner_key=$1 AND closure_id IS NOT NULL),
 EXISTS(SELECT 1 FROM favorites.folders WHERE owner_key=$1 AND is_default)`, owner).Scan(&closed, &initialized)
	if err != nil {
		return nil, false, safeOp("folders.state", err)
	}
	if closed {
		return nil, false, f.WithReason(f.ErrForbidden, f.ReasonOwnerClosed)
	}
	if !initialized {
		return nil, false, nil
	}
	result, err := (&transaction{ctx: ctx, tx: tx, owner: owner}).Folders(pos, limit)
	if err != nil {
		return nil, false, err
	}
	return result, true, safeOp("folders.commit_read", tx.Commit())
}

func (r *Repository) ReadFolders(ctx context.Context, owner, after string, limit int) ([]f.Folder, bool, error) {
	rows, e := r.DB.QueryContext(ctx, `WITH state AS (SELECT COALESCE((SELECT closure_id IS NOT NULL FROM favorites.owners WHERE owner_key=$1),false) AS closed,EXISTS(SELECT 1 FROM favorites.folders WHERE owner_key=$1 AND is_default) AS initialized) SELECT state.closed,state.initialized,folder.id,folder.name,folder.is_default,folder.revision,folder.item_count FROM state LEFT JOIN LATERAL (SELECT id,name,is_default,revision,(SELECT count(*) FROM favorites.items item WHERE item.owner_key=favorites.folders.owner_key AND item.folder_id=favorites.folders.id) AS item_count FROM favorites.folders WHERE NOT state.closed AND state.initialized AND owner_key=$1 AND id>$2 ORDER BY id LIMIT $3) folder ON true`, owner, after, limit)
	if e != nil {
		return nil, false, safe(e)
	}
	defer rows.Close()
	out := []f.Folder{}
	initialized := false
	for rows.Next() {
		var closed bool
		var id, name sql.NullString
		var isDefault sql.NullBool
		var revision, itemCount sql.NullInt64
		if e = rows.Scan(&closed, &initialized, &id, &name, &isDefault, &revision, &itemCount); e != nil {
			return nil, false, safe(e)
		}
		if closed {
			return nil, false, f.WithReason(f.ErrForbidden, f.ReasonOwnerClosed)
		}
		if !initialized {
			return nil, false, nil
		}
		if id.Valid {
			out = append(out, f.Folder{ID: id.String, Name: name.String, Default: isDefault.Bool, Revision: revision.Int64, ItemCount: itemCount.Int64})
		}
	}
	return out, initialized, safe(rows.Err())
}
func (r *Repository) ReadItems(ctx context.Context, owner string, filter f.Filter, pos f.Position) ([]f.Item, error) {
	order := `created_at DESC,id`
	page := `($5='' OR created_at<$6 OR (created_at=$6 AND id>$5))`
	if filter.Sort == "name" {
		order = `title COLLATE "C",id`
		page = `($5='' OR title COLLATE "C">$7 COLLATE "C" OR (title=$7 AND id>$5))`
	}
	rows, e := r.DB.QueryContext(ctx, `WITH state AS (SELECT COALESCE((SELECT closure_id IS NOT NULL FROM favorites.owners WHERE owner_key=$1),false) AS closed,($2='' OR EXISTS(SELECT 1 FROM favorites.folders WHERE owner_key=$1 AND id=$2)) AS folder_ok) SELECT state.closed,state.folder_ok,item.id,item.kind,item.target_id,item.title,item.folder_id,item.revision,item.created_at,item.canonical_target_id FROM state LEFT JOIN LATERAL (SELECT `+columns+` FROM favorites.items WHERE NOT state.closed AND state.folder_ok AND owner_key=$1 AND ($2='' OR folder_id=$2) AND ($3='' OR kind=$3) AND ($4='' OR strpos(lower(title),lower($4))>0) AND `+page+` AND $6::timestamptz IS NOT NULL AND $7::text IS NOT NULL AND ($9::text[] IS NULL OR target_id=ANY($9::text[])) ORDER BY `+order+` LIMIT $8) item ON true`, owner, filter.FolderID, filter.Kind, filter.Query, pos.ID, pos.CreatedAt, pos.Title, filter.Limit, filter.TargetIDs)
	if e != nil {
		return nil, safe(e)
	}
	defer rows.Close()
	out := []f.Item{}
	for rows.Next() {
		var closed, folderOK bool
		var id, kind, targetID, title, folderID, canonical sql.NullString
		var revision sql.NullInt64
		var createdAt sql.NullTime
		if e = rows.Scan(&closed, &folderOK, &id, &kind, &targetID, &title, &folderID, &revision, &createdAt, &canonical); e != nil {
			return nil, safe(e)
		}
		if closed {
			return nil, f.WithReason(f.ErrForbidden, f.ReasonOwnerClosed)
		}
		if !folderOK {
			return nil, f.ErrNotFound
		}
		if id.Valid {
			out = append(out, f.Item{ID: id.String, Kind: kind.String, TargetID: targetID.String, Title: title.String, FolderID: folderID.String, Revision: revision.Int64, CreatedAt: createdAt.Time, CanonicalTargetID: canonical.String})
		}
	}
	return out, safe(rows.Err())
}
