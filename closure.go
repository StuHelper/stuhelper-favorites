package favorites

import "context"

// CloseOwner permanently removes personal collections through a trusted port.
// The closure marker prevents concurrent writes and migration from restoring data.
func (s *Service) CloseOwner(ctx context.Context, owner, id string) error {
	if !valid(owner, 2048) || len(id) != 36 {
		return ErrInput
	}
	r, ok := s.Repo.(interface {
		CloseOwner(context.Context, string, string) error
	})
	if !ok {
		return ErrUnavailable
	}
	return r.CloseOwner(ctx, owner, id)
}
