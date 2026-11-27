package favorites

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
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

// ExportPosition tracks the two independent collections carried by a single page.
type ExportPosition struct {
	Position
	FolderAfter       string `json:",omitempty"`
	LastFolderDefault bool   `json:",omitempty"`
	FoldersDone       bool   `json:",omitempty"`
	ItemsDone         bool   `json:",omitempty"`
}

type ExportReader interface {
	ExportFavorites(ctx context.Context, owner, folderID, kind string, pos ExportPosition, limit int) ([]Folder, []ExportItem, error)
}

type ExportAddressSource interface {
	VisibleAddresses(ctx context.Context, owner, kind string, ids []string) (map[string]string, error)
}

func (s *Service) exportPaths(ctx context.Context, owner string, items []ExportItem) error {
	for i, item := range items {
		if item.Kind == "resource" {
			items[i].TargetPath = TargetPath("resource", item.CanonicalTargetID)
		}
	}
	for _, kind := range []string{"course", "teacher"} {
		positions := map[string][]int{}
		var ids []string
		for i, item := range items {
			if item.Kind != kind {
				continue
			}
			if _, seen := positions[item.CanonicalTargetID]; !seen {
				ids = append(ids, item.CanonicalTargetID)
			}
			positions[item.CanonicalTargetID] = append(positions[item.CanonicalTargetID], i)
		}
		for batch := range slices.Chunk(ids, 100) {
			port, ok := s.Source.(ExportAddressSource)
			if !ok {
				return ErrUnavailable
			}
			call, cancel := context.WithTimeout(ctx, 3*time.Second)
			addresses, err := port.VisibleAddresses(call, owner, kind, batch)
			cancel()
			if errors.Is(err, ErrForbidden) {
				continue
			}
			if err != nil {
				return err
			}
			for _, id := range batch {
				if address := addresses[id]; address != "" {
					for _, i := range positions[id] {
						items[i].TargetPath = TargetPath(kind, address)
					}
				}
			}
		}
	}
	return nil
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
	pos := ExportPosition{}
	hash := digest([]any{"export", owner, f.FolderID, f.Kind})
	if f.After != "" {
		if len(f.After) > 4096 {
			return out, ErrInput
		}
		b, e := base64.RawURLEncoding.DecodeString(f.After)
		if e != nil || json.Unmarshal(b, &pos) != nil || pos.Filter != hash {
			return out, ErrInput
		}
		// A cursor emitted before folder pagination already delivered all folders.
		if !pos.FoldersDone && pos.FolderAfter == "" && pos.ID != "" {
			pos.FoldersDone = true
		}
		if pos.FoldersDone && pos.ItemsDone || (!pos.ItemsDone && (!valid(pos.ID, 200) || pos.CreatedAt.IsZero())) || (!pos.FoldersDone && !valid(pos.FolderAfter, 200)) {
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
	pos.Filter = hash
	pos.FoldersDone = len(folders) <= f.Limit
	if !pos.FoldersDone {
		folders = folders[:f.Limit]
		last := folders[len(folders)-1]
		pos.FolderAfter, pos.LastFolderDefault = last.ID, last.Default
	}
	out.Folders = folders
	pos.ItemsDone = len(items) <= f.Limit
	if len(items) > f.Limit {
		items = items[:f.Limit]
		last := items[len(items)-1]
		pos.ID, pos.CreatedAt = last.ID, last.CreatedAt
	}
	if !pos.FoldersDone || !pos.ItemsDone {
		b, err := json.Marshal(pos)
		if err != nil {
			return out, ErrUnavailable
		}
		out.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	if err := s.exportPaths(ctx, owner, items); err != nil {
		return Export{}, err
	}
	out.Items = items
	return out, nil
}
