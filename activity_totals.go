package favorites

import (
	"context"
	"time"
)

type ActivityTotal struct {
	TargetID string
	Count    int64
}
type ActivityTotalsReader interface {
	ActivityTotals(context.Context, string, time.Time, time.Time) ([]ActivityTotal, error)
}

// DirectoryActivitySnapshot is an in-process aggregate port like
// ActivitySnapshot. It contains no owner or favorite detail. Its consumer must
// join only currently readable catalog targets before returning any result.
// Counts use the existing daily net-addition rule, clamped per day at zero.
func (s *Service) DirectoryActivitySnapshot(ctx context.Context, kind string, from, until time.Time) ([]ActivityTotal, error) {
	if kind != "course" && kind != "teacher" || !ActivityDay(from) || !ActivityDay(until) || !from.Before(until) || until.Sub(from) > 366*24*time.Hour {
		return nil, ErrInput
	}
	repo, ok := s.Repo.(ActivityTotalsReader)
	if !ok {
		return nil, ErrUnavailable
	}
	return repo.ActivityTotals(ctx, kind, from, until)
}
