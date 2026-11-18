package favorites

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	DefaultMaxFolders = 200
	DefaultMaxItems   = 10000
)

var (
	ErrInput           = errors.New("invalid_input")
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
	ErrNotFound        = errors.New("not_found")
	ErrConflict        = errors.New("conflict")
	ErrUnavailable     = errors.New("unavailable")
)

type KindCounts struct {
	Course   int64 `json:"course"`
	Teacher  int64 `json:"teacher"`
	Resource int64 `json:"resource"`
}
type Folder struct {
	CreatedAt  time.Time  `json:"-"`
	KindCounts KindCounts `json:"kind_counts"`
	ItemCount  int64      `json:"item_count"`
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Revision   int64      `json:"revision"`
	Position   int        `json:"-"`
	Default    bool       `json:"is_default"`
}
type Item struct {
	CanonicalTargetID string    `json:"-"`
	ID                string    `json:"id"`
	Kind              string    `json:"kind"`
	TargetID          string    `json:"target_id"`
	Title             string    `json:"title"`
	FolderID          string    `json:"folder_id"`
	Revision          int64     `json:"revision"`
	CreatedAt         time.Time `json:"created_at"`
}
type Folders struct {
	Items []Folder `json:"items"`
	Next  string   `json:"next,omitempty"`
}
type Items struct {
	Items []Item `json:"items"`
	Next  string `json:"next,omitempty"`
}
type Add struct {
	Kind     string `json:"kind"`
	TargetID string `json:"target_id"`
	FolderID string `json:"folder_id,omitempty"`
}
type Target struct {
	Kind     string `json:"kind"`
	TargetID string `json:"target_id"`
}
type Rename struct {
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}
type Move struct {
	FolderID string `json:"folder_id"`
	Revision int64  `json:"revision"`
}
type Removed struct {
	ID      string `json:"id"`
	Removed bool   `json:"removed"`
}
type BatchMove struct {
	FolderID string   `json:"folder_id"`
	IDs      []string `json:"ids"`
}
type BatchRemove struct {
	IDs []string `json:"ids"`
}
type BatchRemoved struct {
	IDs          []string `json:"ids"`
	RemovedCount int64    `json:"removed_count"`
}
type Filter struct {
	TargetIDs                          []string
	FolderID, Kind, Query, Sort, After string
	Limit                              int
}
type Position struct {
	ID        string
	Title     string
	CreatedAt time.Time
	Filter    string
}
type FolderPosition struct {
	ID        string
	Position  int
	CreatedAt time.Time
	Default   bool
	Owner     string
}

// Source must enforce current admission and target visibility before returning a minimal title.
// ResolvedTarget includes the current canonical ID supplied by the owning module.
type ResolvedTarget struct{ ID, Kind, Title string }
type Source interface {
	Resolve(context.Context, string, string, string) (ResolvedTarget, error)
}
type Repository interface {
	Transaction(context.Context, string, func(Transaction) error) error
}
type Transaction interface {
	Replay(string) (string, json.RawMessage, error)
	SaveReplay(string, string, json.RawMessage) error
	Event(string, string) error
	Folder(string) (Folder, error)
	FolderCount() (int, error)
	DefaultFolder() (Folder, error)
	Folders(FolderPosition, int) ([]Folder, error)
	SaveFolder(Folder) error
	DeleteFolder(string, string) error
	Item(string) (Item, error)
	ItemCount() (int, error)
	Target(string, string) (Item, error)
	Items(Filter, Position) ([]Item, error)
	SaveItem(Item) error
	MoveItems([]string, string) ([]Item, error)
	DeleteItem(string, time.Time) error
	DeleteItems([]string, time.Time) ([]string, error)
}
