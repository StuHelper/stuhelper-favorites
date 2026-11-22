package postgres

import (
	"context"
	"time"

	f "github.com/StuHelper/stuhelper-favorites"
)

func (r *Repository) ActivityTotals(ctx context.Context, kind string, from, until time.Time) ([]f.ActivityTotal, error) {
	if kind != "course" && kind != "teacher" || !f.ActivityWindow(from, until) {
		return nil, f.ErrInput
	}
	if !r.readyDB() {
		return nil, f.ErrUnavailable
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT target_id,sum(GREATEST(first_count,0)) FROM favorites.activity_buckets
 WHERE kind=$1 AND day>=$2::text::date AND day<$3::text::date AND visible GROUP BY target_id ORDER BY target_id LIMIT 100001`, kind, f.ActivityDate(from), f.ActivityDate(until))
	if err != nil {
		return nil, safe(err)
	}
	defer rows.Close()
	out := []f.ActivityTotal{}
	for rows.Next() {
		var value f.ActivityTotal
		if err := rows.Scan(&value.TargetID, &value.Count); err != nil {
			return nil, safe(err)
		}
		if value.Count < 0 || len(out) >= 100000 {
			return nil, f.ErrUnavailable
		}
		out = append(out, value)
	}
	return out, safe(rows.Err())
}
