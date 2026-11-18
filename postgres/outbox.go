package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	m "github.com/StuHelper/stuhelper-favorites"
	"time"
)

var (
	_ m.OutboxReader   = (*Repository)(nil)
	_ m.OutboxObserver = (*Repository)(nil)
)

func (r *Repository) OutboxLag(ctx context.Context) (m.OutboxLag, error) {
	var count int64
	var oldest sql.NullTime
	e := r.DB.QueryRowContext(ctx, `SELECT count(*),min(created_at) FROM favorites.outbox WHERE sent_at IS NULL`).Scan(&count, &oldest)
	if e != nil {
		return m.OutboxLag{}, safe(e)
	}
	if count == 0 || !oldest.Valid {
		return m.OutboxLag{}, nil
	}
	at := oldest.Time.UTC()
	return m.OutboxLag{
		PendingCount:    count,
		OldestPendingAt: at,
		OldestAge:       max(0, time.Now().UTC().Sub(at)),
	}, nil
}

func (r *Repository) PendingOutbox(ctx context.Context, limit int, excludeSubjects []string) ([]m.Envelope, error) {
	if limit < 1 || limit > 100 || len(excludeSubjects) > 1000 {
		return nil, m.ErrInput
	}
	if !r.readyDB() {
		return nil, m.ErrUnavailable
	}
	rows, e := r.DB.QueryContext(ctx, `SELECT data FROM favorites.outbox WHERE sent_at IS NULL AND NOT ((data->>'subject')=ANY(COALESCE($2::text[],ARRAY[]::text[]))) ORDER BY seq LIMIT $1`, limit, excludeSubjects)
	if e != nil {
		return nil, safe(e)
	}
	defer rows.Close()
	out := []m.Envelope{}
	for rows.Next() {
		var raw []byte
		var v m.Envelope
		if rows.Scan(&raw) != nil || json.Unmarshal(raw, &v) != nil {
			return nil, m.ErrUnavailable
		}
		out = append(out, v)
	}
	return out, safe(rows.Err())
}
func (r *Repository) ConfirmOutbox(ctx context.Context, id string) error {
	if id == "" || len(id) > 200 {
		return m.ErrInput
	}
	if !r.readyDB() {
		return m.ErrUnavailable
	}
	res, e := r.DB.ExecContext(ctx, `UPDATE favorites.outbox SET sent_at=COALESCE(sent_at,now()) WHERE id=$1`, id)
	if e != nil {
		return safe(e)
	}
	n, e := res.RowsAffected()
	if e != nil {
		return safe(e)
	}
	if n == 0 {
		return m.ErrNotFound
	}
	return nil
}
func (t *transaction) notifyOutbox() error {
	_, e := t.tx.ExecContext(t.ctx, `SELECT pg_notify($1,'')`, m.OutboxChannel)
	return safe(e)
}
