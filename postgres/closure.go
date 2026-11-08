package postgres

import (
	"context"
	"database/sql"
	f "github.com/StuHelper/stuhelper-favorites"
)

func (r *Repository) CloseOwner(ctx context.Context, owner, id string) error {
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
	var saved sql.NullString
	if e = tx.QueryRowContext(ctx, `SELECT closure_id FROM favorites.owners WHERE owner_key=$1`, owner).Scan(&saved); e != nil {
		return safe(e)
	}
	if saved.Valid {
		if saved.String != id {
			return f.ErrConflict
		}
		return nil
	}
	for _, table := range []string{"favorites.items", "favorites.folders", "favorites.requests"} {
		if _, e = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE owner_key=$1`, owner); e != nil {
			return safe(e)
		}
	}
	if _, e = tx.ExecContext(ctx, `UPDATE favorites.owners SET closure_id=$2 WHERE owner_key=$1`, owner, id); e != nil {
		return safe(e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO favorites.events(owner_key,kind,operation) VALUES($1,'owner.close',$2)`, owner, id); e != nil {
		return safe(e)
	}
	return safe(tx.Commit())
}
