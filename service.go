package favorites

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

type Service struct {
	Repo   Repository
	Source Source
	Now    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func valid(s string, max int) bool {
	return s != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Cs)
	})
}
func validKey(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
func normalizeFolderName(raw string) (string, bool) {
	name := strings.TrimSpace(norm.NFC.String(raw))
	if !valid(name, 20) || strings.ContainsFunc(name, func(r rune) bool { return unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Cs) }) {
		return "", false
	}
	return name, true
}
func kind(s string) bool { return s == "course" || s == "teacher" || s == "resource" }
func id() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func mutation[T any](ctx context.Context, s *Service, owner, key, op string, in any, run func(Transaction) (T, error)) (T, error) {
	return mutationWithLegacyInput(ctx, s, owner, key, op, in, nil, run)
}
func mutationWithLegacyInput[T any](ctx context.Context, s *Service, owner, key, op string, in, legacy any, run func(Transaction) (T, error)) (T, error) {
	var out T
	if !valid(owner, 2048) {
		return out, ErrUnauthenticated
	}
	if !validKey(key) {
		return out, ErrInput
	}
	if e := s.ensure(ctx, owner); e != nil {
		return out, e
	}
	h := digest([]any{op, in})
	e := s.Repo.Transaction(ctx, owner, func(tx Transaction) error {
		saved, b, e := tx.Replay(key)
		if e == nil {
			// Receipts written before folder names were normalized hashed the trimmed input.
			if saved != h && (legacy == nil || saved != digest([]any{op, legacy})) {
				return ErrConflict
			}
			if json.Unmarshal(b, &out) != nil {
				return ErrUnavailable
			}
			return nil
		}
		if !errors.Is(e, ErrNotFound) {
			return e
		}
		out, e = run(tx)
		if e != nil {
			return e
		}
		b, e = json.Marshal(out)
		if e != nil {
			return ErrUnavailable
		}
		if e = tx.SaveReplay(key, h, b); e != nil {
			return e
		}
		return tx.Event(op, key)
	})
	return out, e
}
func (s *Service) CreateFolder(ctx context.Context, owner, key, name string) (Folder, error) {
	normalized, ok := normalizeFolderName(name)
	if !ok {
		return Folder{}, ErrInput
	}
	return mutationWithLegacyInput(ctx, s, owner, key, "folder.create", normalized, strings.TrimSpace(name), func(tx Transaction) (Folder, error) {
		v := Folder{ID: id(), Name: normalized, Revision: 1}
		return v, tx.SaveFolder(v)
	})
}
func (s *Service) RenameFolder(ctx context.Context, owner, key, folder string, in Rename) (Folder, error) {
	normalized, ok := normalizeFolderName(in.Name)
	if !valid(folder, 200) || !ok || in.Revision < 1 {
		return Folder{}, ErrInput
	}
	legacy := in
	legacy.Name = strings.TrimSpace(in.Name)
	in.Name = normalized
	return mutationWithLegacyInput(ctx, s, owner, key, "folder.rename", []any{folder, in}, []any{folder, legacy}, func(tx Transaction) (Folder, error) {
		v, e := tx.Folder(folder)
		if e != nil {
			return v, e
		}
		if v.Revision != in.Revision {
			return v, ErrConflict
		}
		v.Name = in.Name
		v.Revision++
		return v, tx.SaveFolder(v)
	})
}
func (s *Service) DeleteFolder(ctx context.Context, owner, key, folder string, revision int64) (Removed, error) {
	if !valid(folder, 200) || revision < 1 {
		return Removed{}, ErrInput
	}
	return mutation(ctx, s, owner, key, "folder.delete", []any{folder, revision}, func(tx Transaction) (Removed, error) {
		v, e := tx.Folder(folder)
		if e != nil {
			return Removed{}, e
		}
		if v.Default {
			return Removed{}, ErrForbidden
		}
		if v.Revision != revision {
			return Removed{}, ErrConflict
		}
		d, e := tx.DefaultFolder()
		if e != nil {
			return Removed{}, e
		}
		return Removed{folder, true}, tx.DeleteFolder(folder, d.ID)
	})
}
func (s *Service) ListFolders(ctx context.Context, owner, after string, limit int) (Folders, error) {
	out := Folders{Items: []Folder{}}
	if limit < 1 || limit > 100 || after != "" && !valid(after, 200) {
		return out, ErrInput
	}
	if e := s.ensure(ctx, owner); e != nil {
		return out, e
	}
	var e error
	if reader, ok := s.Repo.(interface {
		ReadFolders(context.Context, string, string, int) ([]Folder, bool, error)
	}); ok {
		var initialized bool
		out.Items, initialized, e = reader.ReadFolders(ctx, owner, after, limit+1)
		if e != nil {
			return out, e
		}
		if !initialized {
			e = s.Repo.Transaction(ctx, owner, func(tx Transaction) error {
				var txErr error
				out.Items, txErr = tx.Folders(after, limit+1)
				return txErr
			})
		}
	} else {
		e = s.Repo.Transaction(ctx, owner, func(tx Transaction) error {
			var txErr error
			out.Items, txErr = tx.Folders(after, limit+1)
			return txErr
		})
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.Next = out.Items[limit-1].ID
	}
	return out, e
}
func (s *Service) Add(ctx context.Context, owner, key string, in Add) (Item, error) {
	if !kind(in.Kind) || !valid(in.TargetID, 200) || strings.TrimSpace(in.TargetID) != in.TargetID || in.FolderID != "" && (!valid(in.FolderID, 200) || strings.TrimSpace(in.FolderID) != in.FolderID) {
		return Item{}, ErrInput
	}
	if !valid(owner, 2048) {
		return Item{}, ErrUnauthenticated
	}
	if !validKey(key) {
		return Item{}, ErrInput
	}
	if e := s.ensure(ctx, owner); e != nil {
		return Item{}, e
	}
	// A replay or an existing item can be handled without a source call. The
	// transaction makes the final decision, including whether a receipt is still live.
	needResolve := false
	if prechecker, ok := s.Repo.(interface {
		NeedsTargetResolution(context.Context, string, string, Add) (bool, error)
	}); ok {
		var err error
		needResolve, err = prechecker.NeedsTargetResolution(ctx, owner, key, in)
		if err != nil {
			return Item{}, err
		}
	}
	var resolved ResolvedTarget
	resolveTarget := func() error {
		if s.Source == nil {
			return ErrUnavailable
		}
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var e error
		resolved, e = s.Source.Resolve(call, owner, in.Kind, in.TargetID)
		if e != nil {
			if ctx.Err() == nil && errors.Is(e, context.DeadlineExceeded) {
				return ErrUnavailable
			}
			return e
		}
		if !valid(resolved.Title, 500) || !valid(resolved.ID, 200) || resolved.Kind != in.Kind {
			return ErrUnavailable
		}
		return nil
	}
	if needResolve {
		if e := resolveTarget(); e != nil {
			return Item{}, e
		}
	}
	errRetryResolve := errors.New("retry_resolve")
	for range 2 {
		item, e := mutation(ctx, s, owner, key, "item.add", in, func(tx Transaction) (Item, error) {
			old, e := tx.Target(in.Kind, in.TargetID)
			if e == nil {
				if in.FolderID != "" && in.FolderID != old.FolderID {
					return old, ErrConflict
				}
				return old, nil
			}
			if !errors.Is(e, ErrNotFound) {
				return Item{}, e
			}
			var folder Folder
			if in.FolderID == "" {
				folder, e = tx.DefaultFolder()
			} else {
				folder, e = tx.Folder(in.FolderID)
			}
			if e != nil {
				return Item{}, e
			}
			if !needResolve {
				return Item{}, errRetryResolve
			}
			if lookup, ok := tx.(interface {
				CanonicalTarget(string, string) (Item, error)
			}); ok {
				old, e := lookup.CanonicalTarget(in.Kind, resolved.ID)
				if e == nil {
					if in.FolderID != "" && in.FolderID != old.FolderID {
						return old, ErrConflict
					}
					return old, nil
				}
				if !errors.Is(e, ErrNotFound) {
					return Item{}, e
				}
			}
			now := s.now()
			if resolved.Kind == "course" || resolved.Kind == "teacher" {
				if _, _, err := ParseActivityID(ActivityID(resolved.Kind, resolved.ID, now)); err != nil {
					return Item{}, ErrUnavailable
				}
			}

			v := Item{ID: id(), Kind: in.Kind, TargetID: in.TargetID, Title: resolved.Title, CanonicalTargetID: resolved.ID, FolderID: folder.ID, Revision: 1, CreatedAt: now}
			return v, tx.SaveItem(v)
		})
		if !errors.Is(e, errRetryResolve) {
			return item, e
		}
		needResolve = true
		if e := resolveTarget(); e != nil {
			return Item{}, e
		}
	}
	return Item{}, ErrUnavailable
}
func (s *Service) Move(ctx context.Context, owner, key, item string, in Move) (Item, error) {
	if !valid(item, 200) || !valid(in.FolderID, 200) || in.Revision < 1 {
		return Item{}, ErrInput
	}
	return mutation(ctx, s, owner, key, "item.move", []any{item, in}, func(tx Transaction) (Item, error) {
		v, e := tx.Item(item)
		if e != nil {
			return v, e
		}
		if v.Revision != in.Revision {
			return v, ErrConflict
		}
		if _, e = tx.Folder(in.FolderID); e != nil {
			return v, e
		}
		v.FolderID = in.FolderID
		v.Revision++
		return v, tx.SaveItem(v)
	})
}
func (s *Service) Remove(ctx context.Context, owner, key, item string, revision int64) (Removed, error) {
	if !valid(item, 200) || revision < 1 {
		return Removed{}, ErrInput
	}
	return mutation(ctx, s, owner, key, "item.remove", []any{item, revision}, func(tx Transaction) (Removed, error) {
		v, e := tx.Item(item)
		if e != nil {
			return Removed{}, e
		}
		if v.Revision != revision {
			return Removed{}, ErrConflict
		}
		return Removed{item, true}, tx.DeleteItem(item, s.now())
	})
}
func (s *Service) List(ctx context.Context, owner string, f Filter) (Items, error) {
	out := Items{Items: []Item{}}
	f.Query = strings.TrimSpace(f.Query)
	if f.Sort == "" {
		f.Sort = "newest"
	}
	if f.Limit < 1 || f.Limit > 100 || f.Kind != "" && !kind(f.Kind) || f.Query != "" && !valid(f.Query, 100) || f.Sort != "newest" && f.Sort != "name" || f.FolderID != "" && !valid(f.FolderID, 200) {
		return out, ErrInput
	}
	if f.TargetIDs != nil {
		if len(f.TargetIDs) == 0 || len(f.TargetIDs) > 100 || f.Kind == "" {
			return out, ErrInput
		}
		for _, target := range f.TargetIDs {
			if !valid(target, 200) || strings.TrimSpace(target) != target {
				return out, ErrInput
			}
		}
		f.TargetIDs = slices.Clone(f.TargetIDs)
		slices.Sort(f.TargetIDs)
		f.TargetIDs = slices.Compact(f.TargetIDs)
	}
	pos := Position{}
	hash := digest([]any{owner, f.FolderID, f.Kind, f.Query, f.Sort, f.TargetIDs})
	if f.After != "" {
		if len(f.After) > 4096 {
			return out, ErrInput
		}
		b, e := base64.RawURLEncoding.DecodeString(f.After)
		if e != nil || json.Unmarshal(b, &pos) != nil || pos.Filter != hash || !valid(pos.ID, 200) || pos.CreatedAt.IsZero() || (pos.Title != "" && !valid(pos.Title, 500)) {
			return out, ErrInput
		}
	}
	if e := s.ensure(ctx, owner); e != nil {
		return out, e
	}
	f.Limit++
	var e error
	if reader, ok := s.Repo.(interface {
		ReadItems(context.Context, string, Filter, Position) ([]Item, error)
	}); ok {
		out.Items, e = reader.ReadItems(ctx, owner, f, pos)
	} else {
		e = s.Repo.Transaction(ctx, owner, func(tx Transaction) error {
			if f.FolderID != "" {
				if _, txErr := tx.Folder(f.FolderID); txErr != nil {
					return txErr
				}
			}
			var txErr error
			out.Items, txErr = tx.Items(f, pos)
			return txErr
		})
	}
	if len(out.Items) >= f.Limit {
		out.Items = out.Items[:f.Limit-1]
		v := out.Items[len(out.Items)-1]
		b, _ := json.Marshal(Position{ID: v.ID, Title: v.Title, CreatedAt: v.CreatedAt, Filter: hash})
		out.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, e
}
