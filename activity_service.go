package favorites

import (
	"context"
	"errors"
	"time"
)

func (s *Service) CurrentActivity(ctx context.Context, principal, id string) (Activity, bool, error) {
	var empty Activity
	if !valid(principal, 2048) {
		return empty, false, ErrUnauthenticated
	}
	target, _, e := ParseActivityID(id)
	if e != nil {
		return empty, false, e
	}
	if s.Source == nil {
		return empty, false, ErrUnavailable
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resolved, e := s.Source.Resolve(call, principal, target.Kind, target.ID)
	if errors.Is(e, ErrForbidden) || errors.Is(e, ErrNotFound) {
		return empty, false, nil
	} else if e != nil {
		return empty, false, e
	}
	if !valid(resolved.ID, 200) || !valid(resolved.Title, 500) || (resolved.Kind != "course" && resolved.Kind != "teacher") {
		return empty, false, ErrUnavailable
	}
	if resolved.ID != target.ID || resolved.Kind != target.Kind {
		return empty, false, nil
	}
	repo, ok := s.Repo.(ActivityReader)
	if !ok {
		return empty, false, ErrUnavailable
	}
	d, e := repo.ReadActivity(ctx, id)
	if errors.Is(e, ErrNotFound) {
		return empty, false, nil
	}
	return d, e == nil, e
}
