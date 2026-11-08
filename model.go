package favorites

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrInput           = errors.New("invalid_input")
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
	ErrNotFound        = errors.New("not_found")
	ErrConflict        = errors.New("conflict")
	ErrUnavailable     = errors.New("unavailable")
)

type Folder struct {
	ItemCount int64  `json:"item_count"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Default   bool   `json:"is_default"`
	Revision  int64  `json:"revision"`
}
type Item struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	TargetID  string    `json:"target_id"`
	Title     string    `json:"title"`
	FolderID  string    `json:"folder_id"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
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
type Filter struct {
	FolderID, Kind, Query, Sort, After string
	Limit                              int
}
type Position struct {
	ID        string
	Title     string
	CreatedAt time.Time
	Filter    string
}

// Source must enforce current admission and target visibility before returning a minimal title.
type Source interface {
	Resolve(context.Context, string, string, string) (string, error)
}
type Repository interface {
	Transaction(context.Context, string, func(Transaction) error) error
}
type Transaction interface {
	Replay(string) (string, json.RawMessage, error)
	SaveReplay(string, string, json.RawMessage) error
	Event(string, string) error
	Folder(string) (Folder, error)
	DefaultFolder() (Folder, error)
	Folders(string, int) ([]Folder, error)
	SaveFolder(Folder) error
	DeleteFolder(string, string) error
	Item(string) (Item, error)
	Target(string, string) (Item, error)
	Items(Filter, Position) ([]Item, error)
	SaveItem(Item) error
	DeleteItem(string) error
}
