package httpapi

import (
	"encoding/json"
	"errors"
	f "github.com/StuHelper/stuhelper-favorites"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type Handler struct {
	Service      *f.Service
	Authenticate func(*http.Request) (string, error)
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if h.Service == nil || h.Authenticate == nil {
		failure(w, f.ErrUnavailable)
		return
	}
	owner, e := h.Authenticate(r)
	if e != nil {
		failure(w, e)
		return
	}
	if owner == "" {
		failure(w, f.ErrUnauthenticated)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/favorites/")
	parts := strings.Split(path, "/")
	key := r.Header.Get("Idempotency-Key")
	ctx := r.Context()
	var out any
	if r.Method == http.MethodGet && len(parts) == 1 {
		q := r.URL.Query()
		limit := 30
		if q.Has("limit") {
			limit, e = strconv.Atoi(q.Get("limit"))
			if e != nil {
				failure(w, f.ErrInput)
				return
			}
		}
		switch parts[0] {
		case "folders":
			out, e = h.Service.ListFolders(ctx, owner, q.Get("after"), limit)
		case "items":
			out, e = h.Service.List(ctx, owner, f.Filter{After: q.Get("after"), Limit: limit, FolderID: q.Get("folder_id"), Kind: q.Get("kind"), Query: q.Get("q"), Sort: q.Get("sort")})
		default:
			e = f.ErrNotFound
		}
	} else if r.Method == http.MethodPost {
		switch {
		case len(parts) == 1 && parts[0] == "folders":
			var in struct {
				Name string `json:"name"`
			}
			if decode(w, r, &in) != nil {
				return
			}
			out, e = h.Service.CreateFolder(ctx, owner, key, in.Name)
		case len(parts) == 1 && parts[0] == "items":
			var in f.Add
			if decode(w, r, &in) != nil {
				return
			}
			out, e = h.Service.Add(ctx, owner, key, in)
		case len(parts) == 3 && parts[0] == "folders" && parts[2] == "rename":
			var in f.Rename
			if decode(w, r, &in) != nil {
				return
			}
			out, e = h.Service.RenameFolder(ctx, owner, key, parts[1], in)
		case len(parts) == 3 && parts[0] == "folders" && parts[2] == "delete":
			var in struct {
				Revision int64 `json:"revision"`
			}
			if decode(w, r, &in) != nil {
				return
			}
			out, e = h.Service.DeleteFolder(ctx, owner, key, parts[1], in.Revision)
		case len(parts) == 3 && parts[0] == "items" && parts[2] == "move":
			var in f.Move
			if decode(w, r, &in) != nil {
				return
			}
			out, e = h.Service.Move(ctx, owner, key, parts[1], in)
		case len(parts) == 3 && parts[0] == "items" && parts[2] == "remove":
			var in struct {
				Revision int64 `json:"revision"`
			}
			if decode(w, r, &in) != nil {
				return
			}
			out, e = h.Service.Remove(ctx, owner, key, parts[1], in.Revision)
		default:
			e = f.ErrNotFound
		}
	} else {
		e = f.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	e := decoder.Decode(v)
	if e == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			e = f.ErrInput
		}
	}
	if e != nil {
		failure(w, f.ErrInput)
	}
	return e
}
func failure(w http.ResponseWriter, e error) {
	code, status := "unavailable", http.StatusServiceUnavailable
	switch {
	case errors.Is(e, f.ErrInput):
		code, status = "invalid_input", 400
	case errors.Is(e, f.ErrUnauthenticated):
		code, status = "unauthenticated", 401
	case errors.Is(e, f.ErrForbidden):
		code, status = "forbidden", 403
	case errors.Is(e, f.ErrNotFound):
		code, status = "not_found", 404
	case errors.Is(e, f.ErrConflict):
		code, status = "conflict", 409
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}
