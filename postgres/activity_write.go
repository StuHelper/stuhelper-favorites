package postgres

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	m "github.com/StuHelper/stuhelper-favorites"
	"time"
)

// Bucket row locks serialize independent actors. Callers update keys in ID order
// when an operation affects multiple buckets, avoiding inverse lock acquisition.
func (t *transaction) changeActivity(kind, target string, day time.Time, delta int64, at time.Time) error {
	day = day.UTC().Truncate(24 * time.Hour)
	id := m.ActivityID(kind, target, day)
	if _, _, e := m.ParseActivityID(id); e != nil {
		return e
	}
	var first, revision int64
	e := t.tx.QueryRowContext(t.ctx, `INSERT INTO favorites.activity_buckets(id,kind,target_id,day,first_count,revision,visible) VALUES($1,$2,$3,$4,$5,1,false) ON CONFLICT(id) DO UPDATE SET first_count=favorites.activity_buckets.first_count+EXCLUDED.first_count,revision=favorites.activity_buckets.revision+1 RETURNING first_count,revision`, id, kind, target, day, delta).Scan(&first, &revision)
	if e != nil {
		return safe(e)
	}
	first = max(0, first)
	if first > 1000000000 {
		return m.ErrUnavailable
	}
	d := m.Activity{Source: "favorites", ID: id, Target: m.ActivityTarget{Source: "catalog", Kind: kind, ID: target}, Day: day.Format(time.DateOnly), Revision: revision, Visible: first > 0, Metrics: map[string]int64{"favorites": first}}
	data, e := json.Marshal(d)
	if e != nil {
		return m.ErrUnavailable
	}
	if _, e = t.tx.ExecContext(t.ctx, `UPDATE favorites.activity_buckets SET visible=$2,data=$3 WHERE id=$1`, id, d.Visible, data); e != nil {
		return safe(e)
	}
	if _, e = t.tx.ExecContext(t.ctx, `INSERT INTO favorites.activity_history(id,revision,day,data) VALUES($1,$2,$3,$4)`, id, revision, day, data); e != nil {
		return safe(e)
	}
	eventID := fmt.Sprintf("activity:%x:%d", sha256.Sum256([]byte(id)), revision)
	return t.saveEnvelope(m.Envelope{SpecVersion: "1.0", ID: eventID, Source: "favorites", Type: "discovery.activity.v1", Subject: id, Time: at, Data: data})
}
