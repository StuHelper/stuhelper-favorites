package postgres

import (
	"context"
	"database/sql"
	f "github.com/StuHelper/stuhelper-favorites"
	"strconv"
	"time"
)

func (r *Repository) CloseOwner(ctx context.Context, owner, id string) error {
	return r.CloseOwnerAt(ctx, owner, id, time.Now().UTC())
}

func (r *Repository) CloseOwnerAt(ctx context.Context, owner, id string, at time.Time) error {
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
	if e := (&transaction{ctx: ctx, tx: tx, owner: owner}).removeOwnerActivity(at.UTC()); e != nil {
		return e
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

// Keep the grouped result in PostgreSQL and fetch a bounded batch at a time.
func (t *transaction) removeOwnerActivity(at time.Time) error {
	const batchSize = 256
	// Sort exactly like ActivityID, including the slash following the unpadded base64url.
	_, err := t.tx.ExecContext(t.ctx, `DECLARE closing_activity NO SCROLL CURSOR FOR
   SELECT kind,target,-count(*) FROM (
       SELECT kind,coalesce(nullif(canonical_target_id,''),target_id) AS target
       FROM favorites.items WHERE owner_key=$1 AND kind IN ('course','teacher')
   ) items GROUP BY kind,target
   ORDER BY (kind || '/' || translate(encode(convert_to(target,'UTF8'),'base64'), '+/=' || chr(10),'-_') || '/') COLLATE "C"`, t.owner)
	if err != nil {
		return safe(err)
	}
	changed := false
	for {
		rows, err := t.tx.QueryContext(t.ctx, "FETCH FORWARD "+strconv.Itoa(batchSize)+" FROM closing_activity")
		if err != nil {
			return safe(err)
		}
		batch := make([]activityDelta, 0, batchSize)
		for rows.Next() {
			var v activityDelta
			if err := rows.Scan(&v.kind, &v.target, &v.delta); err != nil {
				rows.Close()
				return safe(err)
			}
			batch = append(batch, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return safe(err)
		}
		if len(batch) > 0 {
			if err := t.changeActivities(batch, "", at, 0, nil, at); err != nil {
				return err
			}
			changed = true
		}
		if len(batch) < batchSize {
			break
		}
	}
	if changed {
		return t.notifyOutbox()
	}
	return nil
}
