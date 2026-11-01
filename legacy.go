package favorites

import (
	"context"
	"errors"
	"time"
)

func (s *Service) ensure(ctx context.Context, owner string) error {
	if !valid(owner, 2048) {
		return ErrUnauthenticated
	}
	if s.Repo == nil {
		return ErrUnavailable
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// At most 2,000 old rows per call. Durable progress survives unavailable sources,
	// unknown commit results and retries; clients retry 503 until all pages are imported.
	for range 20 {
		var after string
		var done bool
		if e := s.Repo.Transaction(call, owner, func(tx Transaction) error { var e error; after, done, e = tx.ImportProgress(); return e }); e != nil {
			return e
		}
		if done {
			return nil
		}
		if s.Legacy == nil {
			return ErrUnavailable
		}
		page, e := s.Legacy.Page(call, owner, after, 100)
		if e != nil {
			return e
		}
		if len(page.Items) > 100 || page.Next != "" && (page.Next <= after || len(page.Items) == 0) {
			return ErrUnavailable
		}
		last := after
		for _, v := range page.Items {
			if !valid(v.TargetID, 200) || v.TargetID <= last || v.Title != "" && !valid(v.Title, 500) {
				return ErrUnavailable
			}
			last = v.TargetID
		}
		if page.Next != "" && page.Next != last {
			return ErrUnavailable
		}

		e = s.Repo.Transaction(call, owner, func(tx Transaction) error {
			current, finished, e := tx.ImportProgress()
			if e != nil {
				return e
			}
			if finished || current != after {
				return nil
			}
			folder, e := tx.DefaultFolder()
			if e != nil {
				return e
			}
			for _, v := range page.Items {
				if _, e = tx.Target("course", v.TargetID); e == nil {
					continue
				}
				if !errors.Is(e, ErrNotFound) {
					return e
				}
				item := Item{ID: id(), Kind: "course", TargetID: v.TargetID, Title: v.Title, FolderID: folder.ID, Revision: 1, CreatedAt: s.now()}
				if e = tx.SaveItem(item); e != nil {
					return e
				}
			}
			if e = tx.AdvanceImport(after, page.Next, page.Next == ""); e != nil {
				return e
			}
			return tx.Event("legacy.import", after)
		})
		if e != nil {
			return e
		}
	}
	return ErrUnavailable
}

// CourseBookmarks backs the former reviews API without a second source of truth.
func (s *Service) CourseBookmarks(ctx context.Context, owner, after string, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInput
	}
	if e := s.ensure(ctx, owner); e != nil {
		return nil, e
	}
	var out []string
	// The repository's legacy-order query is deliberately a separate narrow port.
	repo, ok := s.Repo.(interface {
		CourseBookmarks(context.Context, string, string, int) ([]string, error)
	})
	if !ok {
		return nil, ErrUnavailable
	}
	out, e := repo.CourseBookmarks(ctx, owner, after, limit)
	return out, e
}
func (s *Service) SetCourseBookmark(ctx context.Context, owner, target, key string, set bool) error {
	if set {
		_, e := s.Add(ctx, owner, key, Add{Kind: "course", TargetID: target})
		return e
	}
	if !valid(target, 200) {
		return ErrInput
	}
	_, e := mutation(ctx, s, owner, key, "legacy.remove", target, func(tx Transaction) (Removed, error) {
		v, e := tx.Target("course", target)
		if errors.Is(e, ErrNotFound) {
			return Removed{target, true}, nil
		}
		if e != nil {
			return Removed{}, e
		}
		return Removed{target, true}, tx.DeleteItem(v.ID)
	})
	return e
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
