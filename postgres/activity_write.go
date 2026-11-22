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
func (t *transaction) changeActivity(kind, target string, delta int64, at time.Time) error {
	if err := t.changeActivities([]activityDelta{{kind: kind, target: target, delta: delta}}, at); err != nil {
		return err
	}
	return t.notifyOutbox()
}

type activityDelta struct {
	kind, target string
	delta        int64
}

// Lock all buckets in protocol order, then persist their snapshots and envelopes together.
func (t *transaction) changeActivities(changes []activityDelta, at time.Time) error {
	day := m.ActivityDate(at)
	ids := make([]string, len(changes))
	kinds := make([]string, len(changes))
	targets := make([]string, len(changes))
	deltas := make([]int64, len(changes))
	for i, change := range changes {
		ids[i] = m.ActivityID(change.kind, change.target, at)
		if _, _, err := m.ParseActivityID(ids[i]); err != nil {
			return err
		}
		kinds[i], targets[i], deltas[i] = change.kind, change.target, change.delta
	}
	rows, e := t.tx.QueryContext(t.ctx, `INSERT INTO favorites.activity_buckets AS b
  (id,kind,target_id,day,first_count,revision,visible)
  SELECT id,kind,target,$5::text::date,delta,1,false
  FROM unnest($1::text[],$2::text[],$3::text[],$4::bigint[]) AS v(id,kind,target,delta)
  ORDER BY id COLLATE "C"
  ON CONFLICT(id) DO UPDATE SET first_count=b.first_count+EXCLUDED.first_count,
    revision=b.revision+1
  RETURNING id,kind,target_id,first_count,revision`, ids, kinds, targets, deltas, day)
	if e != nil {
		return safe(e)
	}
	defer rows.Close()
	envelopes := make([]m.Envelope, 0, len(changes))
	for rows.Next() {
		var id, kind, target string
		var first, revision int64
		if e := rows.Scan(&id, &kind, &target, &first, &revision); e != nil {
			return safe(e)
		}
		first = max(0, first)
		if first > 1000000000 {
			return m.ErrUnavailable
		}
		d := m.Activity{Source: "favorites", ID: id, Target: m.ActivityTarget{Source: "catalog", Kind: kind, ID: target}, Day: day, Revision: revision, Visible: first > 0, Metrics: map[string]int64{"favorites": first}}
		data, e := json.Marshal(d)
		if e != nil {
			return m.ErrUnavailable
		}
		eventID := fmt.Sprintf("activity:%x:%d", sha256.Sum256([]byte(id)), revision)
		envelopes = append(envelopes, m.Envelope{SpecVersion: "1.0", ID: eventID, Source: "favorites", Type: m.ActivityEventType, Subject: id, Time: at, Data: data})
	}
	if e := rows.Err(); e != nil {
		return safe(e)
	}
	raw, e := json.Marshal(envelopes)
	if e != nil {
		return m.ErrUnavailable
	}
	_, e = t.tx.ExecContext(t.ctx, `WITH snapshots AS (
    SELECT v AS envelope,v->'data' AS data FROM jsonb_array_elements($1::jsonb) v
 ), updated AS (
    UPDATE favorites.activity_buckets b SET visible=(s.data->>'visible')::boolean,data=s.data
    FROM snapshots s WHERE b.id=s.data->>'id'
 ), archived AS (
    INSERT INTO favorites.activity_history(id,revision,day,data)
    SELECT data->>'id',(data->>'revision')::bigint,(data->>'day')::date,data FROM snapshots
 )
 INSERT INTO favorites.outbox(id,data,created_at)
 SELECT envelope->>'id',envelope,(envelope->>'time')::timestamptz FROM snapshots ORDER BY (envelope->>'subject') COLLATE "C"`, raw)
	return safe(e)
}
