package favorites

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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
	return s != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsFunc(s, unicode.IsControl)
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
	var out T
	if !valid(owner, 2048) {
		return out, ErrUnauthenticated
	}
	if !valid(key, 128) {
		return out, ErrInput
	}
	if e := s.ensure(ctx, owner); e != nil {
		return out, e
	}
	h := digest([]any{op, in})
	e := s.Repo.Transaction(ctx, owner, func(tx Transaction) error {
		saved, b, e := tx.Replay(key)
		if e == nil {
			if saved != h {
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
	name = strings.TrimSpace(name)
	if !valid(name, 20) {
		return Folder{}, ErrInput
	}
	return mutation(ctx, s, owner, key, "folder.create", name, func(tx Transaction) (Folder, error) {
		v := Folder{ID: id(), Name: name, Revision: 1}
		return v, tx.SaveFolder(v)
	})
}
func (s *Service) RenameFolder(ctx context.Context, owner, key, folder string, in Rename) (Folder, error) {
	in.Name = strings.TrimSpace(in.Name)
	if !valid(folder, 200) || !valid(in.Name, 20) || in.Revision < 1 {
		return Folder{}, ErrInput
	}
	return mutation(ctx, s, owner, key, "folder.rename", []any{folder, in}, func(tx Transaction) (Folder, error) {
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
	e := s.Repo.Transaction(ctx, owner, func(tx Transaction) error { var e error; out.Items, e = tx.Folders(after, limit+1); return e })
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.Next = out.Items[limit-1].ID
	}
	return out, e
}
func (s *Service) Add(ctx context.Context, owner, key string, in Add) (Item, error) {
	if !kind(in.Kind) || !valid(in.TargetID, 200) || in.FolderID != "" && !valid(in.FolderID, 200) {
		return Item{}, ErrInput
	}
	return mutation(ctx, s, owner, key, "item.add", in, func(tx Transaction) (Item, error) {
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
		if s.Source == nil {
			return Item{}, ErrUnavailable
		}
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		title, e := s.Source.Resolve(call, owner, in.Kind, in.TargetID)
		if e != nil {
			return Item{}, e
		}
		if !valid(title, 500) {
			return Item{}, ErrUnavailable
		}
		v := Item{ID: id(), Kind: in.Kind, TargetID: in.TargetID, Title: title, FolderID: folder.ID, Revision: 1, CreatedAt: s.now()}
		return v, tx.SaveItem(v)
	})
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
		return Removed{item, true}, tx.DeleteItem(item)
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
	pos := Position{}
	hash := digest([]string{owner, f.FolderID, f.Kind, f.Query, f.Sort})
	if f.After != "" {
		if len(f.After) > 4096 {
			return out, ErrInput
		}
		b, e := base64.RawURLEncoding.DecodeString(f.After)
		if e != nil || json.Unmarshal(b, &pos) != nil || pos.Filter != hash || !valid(pos.ID, 200) {
			return out, ErrInput
		}
	}
	if e := s.ensure(ctx, owner); e != nil {
		return out, e
	}
	e := s.Repo.Transaction(ctx, owner, func(tx Transaction) error {
		if f.FolderID != "" {
			if _, e := tx.Folder(f.FolderID); e != nil {
				return e
			}
		}
		var e error
		f.Limit++
		out.Items, e = tx.Items(f, pos)
		return e
	})
	if len(out.Items) >= f.Limit {
		out.Items = out.Items[:f.Limit-1]
		v := out.Items[len(out.Items)-1]
		b, _ := json.Marshal(Position{ID: v.ID, Title: v.Title, CreatedAt: v.CreatedAt, Filter: hash})
		out.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, e
}
