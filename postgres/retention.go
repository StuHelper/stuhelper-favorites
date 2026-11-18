package postgres

import (
	"context"

	f "github.com/StuHelper/stuhelper-favorites"
)

// PruneExpired removes at most limit rows per class and returns their committed total.
// Run with a dedicated cleaner. Pending deliveries have no expiration timestamp.
func (r *Repository) PruneExpired(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, f.ErrInput
	}
	if r == nil || r.DB == nil {
		return 0, f.ErrUnavailable
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, safe(err)
	}
	defer tx.Rollback()
	var total int64
	for _, query := range []string{
		`DELETE FROM favorites.requests r USING (
     SELECT owner_key,key,created_at FROM favorites.requests WHERE created_at <= now()-interval '24 hours'
     ORDER BY created_at,owner_key,key LIMIT $1
   ) expired WHERE (r.owner_key,r.key,r.created_at)=(expired.owner_key,expired.key,expired.created_at)`,
		`DELETE FROM favorites.outbox o USING (
     SELECT id FROM favorites.outbox WHERE sent_at <= (now() AT TIME ZONE 'UTC' - interval '90 days') AT TIME ZONE 'UTC'
     ORDER BY sent_at,id LIMIT $1
   ) expired WHERE o.id=expired.id`,
		`DELETE FROM favorites.events e USING (
     SELECT id FROM favorites.events WHERE created_at <= (now() AT TIME ZONE 'UTC' - interval '12 months') AT TIME ZONE 'UTC'
     ORDER BY created_at,id LIMIT $1
   ) expired WHERE e.id=expired.id`,
	} {
		res, err := tx.ExecContext(ctx, query, limit)
		if err != nil {
			return 0, safe(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, safe(err)
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, safe(err)
	}
	return total, nil
}
