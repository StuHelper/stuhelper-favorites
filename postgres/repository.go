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
)

//go:embed schema.sql
var Schema string

type Repository struct{ DB *sql.DB }
type transaction struct {
	ctx   context.Context
	tx    *sql.Tx
	owner string
}

func safe(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, sql.ErrNoRows) {
		return f.ErrNotFound
	}
	if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return e
	}
	var p *pgconn.PgError
	if errors.As(e, &p) && (p.Code == "23505" || p.Code == "23503" || p.Code == "40001" || p.Code == "40P01") {
		return f.ErrConflict
	}
	return f.ErrUnavailable
}
func Migrate(ctx context.Context, db *sql.DB) error {
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
	return tx.Commit()
}
func (r *Repository) Ready(ctx context.Context) error {
	var exists bool
	e := r.DB.QueryRowContext(ctx, `SELECT to_regclass('favorites.items') IS NOT NULL AND to_regclass('favorites.owners') IS NOT NULL`).Scan(&exists)
	if e != nil || !exists {
		return f.ErrUnavailable
	}
	return nil
}
func (r *Repository) CheckRuntime(ctx context.Context) error {
	var unsafe bool
	e := r.DB.QueryRowContext(ctx, `SELECT rolsuper OR rolcreaterole OR rolcreatedb OR has_schema_privilege(current_user,'favorites','CREATE') OR has_table_privilege(current_user,'favorites.events','UPDATE,DELETE') OR has_table_privilege(current_user,'favorites.requests','UPDATE') FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe)
	if e != nil || unsafe {
		return f.ErrUnavailable
	}
	return r.Ready(ctx)
}
func (r *Repository) Transaction(ctx context.Context, owner string, run func(f.Transaction) error) error {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return safe(e)
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "favorites:"+owner); e != nil {
		return safe(e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO favorites.owners(owner_key) VALUES($1) ON CONFLICT DO NOTHING`, owner); e != nil {
		return safe(e)
	}
	var closed bool
	if e = tx.QueryRowContext(ctx, `SELECT closure_id IS NOT NULL FROM favorites.owners WHERE owner_key=$1`, owner).Scan(&closed); e != nil {
		return safe(e)
	}
	if closed {
		return f.ErrForbidden
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO favorites.folders(owner_key,id,name,is_default,revision) SELECT $1,gen_random_uuid()::text,'',true,1 WHERE NOT EXISTS(SELECT 1 FROM favorites.folders WHERE owner_key=$1 AND is_default)`, owner); e != nil {
		return safe(e)
	}
	if e = run(&transaction{ctx, tx, owner}); e != nil {
		return e
	}
	return safe(tx.Commit())
}
func (t *transaction) Replay(key string) (string, json.RawMessage, error) {
	var h string
	var b []byte
	e := t.tx.QueryRowContext(t.ctx, `SELECT digest,result FROM favorites.requests WHERE owner_key=$1 AND key=$2`, t.owner, key).Scan(&h, &b)
	return h, b, safe(e)
}
func (t *transaction) SaveReplay(key, h string, b json.RawMessage) error {
	_, e := t.tx.ExecContext(t.ctx, `INSERT INTO favorites.requests VALUES($1,$2,$3,$4)`, t.owner, key, h, string(b))
	return safe(e)
}
func (t *transaction) Event(kind, key string) error {
	_, e := t.tx.ExecContext(t.ctx, `INSERT INTO favorites.events(owner_key,kind,operation) VALUES($1,$2,$3)`, t.owner, kind, key)
	return safe(e)
}

type scanner interface{ Scan(...any) error }

func folder(row scanner) (f.Folder, error) {
	var v f.Folder
	e := row.Scan(&v.ID, &v.Name, &v.Default, &v.Revision, &v.ItemCount)
	return v, safe(e)
}
func (t *transaction) Folder(id string) (f.Folder, error) {
	return folder(t.tx.QueryRowContext(t.ctx, `SELECT id,name,is_default,revision,(SELECT count(*) FROM favorites.items item WHERE item.owner_key=favorites.folders.owner_key AND item.folder_id=favorites.folders.id) FROM favorites.folders WHERE owner_key=$1 AND id=$2`, t.owner, id))
}
func (t *transaction) DefaultFolder() (f.Folder, error) {
	return folder(t.tx.QueryRowContext(t.ctx, `SELECT id,name,is_default,revision,(SELECT count(*) FROM favorites.items item WHERE item.owner_key=favorites.folders.owner_key AND item.folder_id=favorites.folders.id) FROM favorites.folders WHERE owner_key=$1 AND is_default`, t.owner))
}
func (t *transaction) Folders(after string, limit int) ([]f.Folder, error) {
	rows, e := t.tx.QueryContext(t.ctx, `SELECT id,name,is_default,revision,(SELECT count(*) FROM favorites.items item WHERE item.owner_key=favorites.folders.owner_key AND item.folder_id=favorites.folders.id) FROM favorites.folders WHERE owner_key=$1 AND id>$2 ORDER BY id LIMIT $3`, t.owner, after, limit)
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
	_, e := t.tx.ExecContext(t.ctx, `INSERT INTO favorites.folders VALUES($1,$2,$3,$4,$5) ON CONFLICT(owner_key,id) DO UPDATE SET name=EXCLUDED.name,revision=EXCLUDED.revision`, t.owner, v.ID, v.Name, v.Default, v.Revision)
	return safe(e)
}
func (t *transaction) DeleteFolder(id, defaultID string) error {
	if _, e := t.tx.ExecContext(t.ctx, `UPDATE favorites.items SET folder_id=$3,revision=revision+1 WHERE owner_key=$1 AND folder_id=$2`, t.owner, id, defaultID); e != nil {
		return safe(e)
	}
	_, e := t.tx.ExecContext(t.ctx, `DELETE FROM favorites.folders WHERE owner_key=$1 AND id=$2 AND NOT is_default`, t.owner, id)
	return safe(e)
}

const columns = `id,kind,target_id,title,folder_id,revision,created_at`

func item(row scanner) (f.Item, error) {
	var v f.Item
	e := row.Scan(&v.ID, &v.Kind, &v.TargetID, &v.Title, &v.FolderID, &v.Revision, &v.CreatedAt)
	return v, safe(e)
}
func (t *transaction) Item(id string) (f.Item, error) {
	return item(t.tx.QueryRowContext(t.ctx, `SELECT `+columns+` FROM favorites.items WHERE owner_key=$1 AND id=$2`, t.owner, id))
}
func (t *transaction) Target(kind, target string) (f.Item, error) {
	return item(t.tx.QueryRowContext(t.ctx, `SELECT `+columns+` FROM favorites.items WHERE owner_key=$1 AND kind=$2 AND target_id=$3`, t.owner, kind, target))
}
func (t *transaction) Items(filter f.Filter, pos f.Position) ([]f.Item, error) {
	order := `created_at DESC,id`
	page := `($5='' OR created_at<$6 OR (created_at=$6 AND id>$5))`
	if filter.Sort == "name" {
		order = `title COLLATE "C",id`
		page = `($5='' OR title COLLATE "C">$7 COLLATE "C" OR (title=$7 AND id>$5))`
	}
	rows, e := t.tx.QueryContext(t.ctx, `SELECT `+columns+` FROM favorites.items WHERE owner_key=$1 AND ($2='' OR folder_id=$2) AND ($3='' OR kind=$3) AND ($4='' OR strpos(lower(title),lower($4))>0) AND `+page+` AND $6::timestamptz IS NOT NULL AND $7::text IS NOT NULL ORDER BY `+order+` LIMIT $8`, t.owner, filter.FolderID, filter.Kind, filter.Query, pos.ID, pos.CreatedAt, pos.Title, filter.Limit)
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
	_, e := t.tx.ExecContext(t.ctx, `INSERT INTO favorites.items(owner_key,`+columns+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(owner_key,id) DO UPDATE SET folder_id=EXCLUDED.folder_id,revision=EXCLUDED.revision`, t.owner, v.ID, v.Kind, v.TargetID, v.Title, v.FolderID, v.Revision, v.CreatedAt)
	return safe(e)
}
func (t *transaction) DeleteItem(id string) error {
	_, e := t.tx.ExecContext(t.ctx, `DELETE FROM favorites.items WHERE owner_key=$1 AND id=$2`, t.owner, id)
	return safe(e)
}
func (r *Repository) CourseReferences(ctx context.Context, owner string, ids []string) ([]string, error) {
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
