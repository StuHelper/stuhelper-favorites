package postgres

import (
	"context"
	"encoding/json"
	m "github.com/StuHelper/stuhelper-favorites"
)

var _ m.OutboxReader = (*Repository)(nil)

func (r *Repository) PendingOutbox(ctx context.Context, limit int, excludeSubjects []string) ([]m.Envelope, error) {
	if limit < 1 || limit > 100 || len(excludeSubjects) > 1000 {
		return nil, m.ErrInput
	}
	rows, e := r.DB.QueryContext(ctx, `SELECT data FROM favorites.outbox WHERE sent_at IS NULL AND NOT ((data->>'subject')=ANY(COALESCE($2::text[],ARRAY[]::text[]))) ORDER BY (data->>'source') COLLATE "C",(data->>'subject') COLLATE "C",seq LIMIT $1`, limit, excludeSubjects)
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
func (t *transaction) saveEnvelope(v m.Envelope) error {
	raw, e := json.Marshal(v)
	if e != nil {
		return m.ErrInput
	}
	if _, e = t.tx.ExecContext(t.ctx, `INSERT INTO favorites.outbox(id,data) VALUES($1,$2)`, v.ID, raw); e != nil {
		return safe(e)
	}
	_, e = t.tx.ExecContext(t.ctx, `SELECT pg_notify($1,'')`, m.OutboxChannel)
	return safe(e)
}
