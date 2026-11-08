package favorites

import "context"

func (s *Service) ensure(_ context.Context, owner string) error {
	if !valid(owner, 2048) {
		return ErrUnauthenticated
	}
	if s.Repo == nil {
		return ErrUnavailable
	}
	return nil
}

func (s *Service) CourseReferences(ctx context.Context, owner string, ids []string) ([]string, error) {
	if len(ids) < 1 || len(ids) > 100 {
		return nil, ErrInput
	}
	for _, id := range ids {
		if !valid(id, 200) {
			return nil, ErrInput
		}
	}
	if e := s.ensure(ctx, owner); e != nil {
		return nil, e
	}
	repo, ok := s.Repo.(interface {
		CourseReferences(context.Context, string, []string) ([]string, error)
	})
	if !ok {
		return nil, ErrUnavailable
	}
	return repo.CourseReferences(ctx, owner, ids)
}
