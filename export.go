package favorites

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"time"
)

type ExportItem struct {
	ID                string    `json:"id"`
	Kind              string    `json:"kind"`
	TargetID          string    `json:"target_id"`
	CanonicalTargetID string    `json:"canonical_target_id"`
	TargetPath        string    `json:"target_path"`
	Title             string    `json:"title"`
	FolderID          string    `json:"folder_id"`
	FolderName        string    `json:"folder_name"`
	CreatedAt         time.Time `json:"created_at"`
}

type Export struct {
	ExportedAt time.Time    `json:"exported_at"`
	Folders    []Folder     `json:"folders"`
	Items      []ExportItem `json:"items"`
	Next       string       `json:"next,omitempty"`
}

type ExportFilter struct {
	FolderID string
	Kind     string
	After    string
	Limit    int
}

type ExportReader interface {
	ExportFavorites(ctx context.Context, owner, folderID, kind string, pos Position, limit int) ([]Folder, []ExportItem, error)
}

func TargetPath(itemKind, canonicalID string) string {
	escaped := url.PathEscape(canonicalID)
	switch itemKind {
	case "course":
		return "/courses/" + escaped
	case "teacher":
		return "/teachers/" + escaped
	case "resource":
		return "/resources/items/" + escaped
	default:
		return ""
	}
}

func (s *Service) Export(ctx context.Context, owner string, f ExportFilter) (Export, error) {
	out := Export{Folders: []Folder{}, Items: []ExportItem{}}
	if e := s.ensure(ctx, owner); e != nil {
		return out, e
	}
	if f.Limit < 1 || f.Limit > 500 || f.Kind != "" && !kind(f.Kind) || f.FolderID != "" && !valid(f.FolderID, 200) {
		return out, ErrInput
	}
	pos := Position{}
	hash := digest([]any{"export", owner, f.FolderID, f.Kind})
	if f.After != "" {
		if len(f.After) > 4096 {
			return out, ErrInput
		}
		b, e := base64.RawURLEncoding.DecodeString(f.After)
		if e != nil || json.Unmarshal(b, &pos) != nil || pos.Filter != hash || !valid(pos.ID, 200) || pos.CreatedAt.IsZero() {
			return out, ErrInput
		}
	}
	reader, ok := s.Repo.(ExportReader)
	if !ok {
		return out, ErrUnavailable
	}
	folders, items, e := reader.ExportFavorites(ctx, owner, f.FolderID, f.Kind, pos, f.Limit+1)
	if e != nil {
		return out, e
	}
	out.ExportedAt = s.now()
	out.Folders = folders
	if len(items) > f.Limit {
		items = items[:f.Limit]
		last := items[len(items)-1]
		b, _ := json.Marshal(Position{ID: last.ID, CreatedAt: last.CreatedAt, Filter: hash})
		out.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	for i := range items {
		items[i].TargetPath = TargetPath(items[i].Kind, items[i].CanonicalTargetID)
	}
	out.Items = items
	return out, nil
}
