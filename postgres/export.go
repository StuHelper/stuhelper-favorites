package postgres

import (
	"context"
	"database/sql"
	"errors"

	f "github.com/StuHelper/stuhelper-favorites"
)

var _ f.ExportReader = (*Repository)(nil)

func (r *Repository) ExportFavorites(ctx context.Context, owner, folderID, kind string, pos f.ExportPosition, limit int) ([]f.Folder, []f.ExportItem, error) {
	tx, e := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return nil, nil, safe(e)
	}
	defer tx.Rollback()

	var closed bool
	e = tx.QueryRowContext(ctx, `SELECT closure_id IS NOT NULL FROM favorites.owners WHERE owner_key=$1`, owner).Scan(&closed)
	if errors.Is(e, sql.ErrNoRows) {
		if folderID != "" {
			return nil, nil, f.ErrNotFound
		}
		return []f.Folder{}, []f.ExportItem{}, safe(tx.Commit())
	}
	if e != nil {
		return nil, nil, safe(e)
	}
	if closed {
		return nil, nil, f.ErrForbidden
	}

	// Page folders before counting their items so a small export page cannot aggregate the entire owner.
	folderRows, e := tx.QueryContext(ctx, `SELECT fld.id,fld.name,fld.is_default,fld.revision,
 (SELECT count(*) FROM favorites.items WHERE owner_key=$1 AND folder_id=fld.id)
 FROM (SELECT id,name,is_default,revision FROM favorites.folders
 WHERE owner_key=$1 AND ($2='' OR id=$2) AND NOT $6
 AND ($3='' OR is_default < $4 OR (is_default=$4 AND id COLLATE "C" > $3 COLLATE "C"))
 ORDER BY is_default DESC,id COLLATE "C" LIMIT $5) fld
 ORDER BY fld.is_default DESC,fld.id COLLATE "C"`,
		owner, folderID, pos.FolderAfter, pos.LastFolderDefault, limit, pos.FoldersDone)
	if e != nil {
		return nil, nil, safe(e)
	}
	folders := []f.Folder{}
	for folderRows.Next() {
		v, err := folder(folderRows)
		if err != nil {
			folderRows.Close()
			return nil, nil, err
		}
		folders = append(folders, v)
	}
	e = folderRows.Err()
	folderRows.Close()
	if e != nil {
		return nil, nil, safe(e)
	}
	if folderID != "" && len(folders) == 0 {
		var exists bool
		if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM favorites.folders WHERE owner_key=$1 AND id=$2)`, owner, folderID).Scan(&exists); e != nil {
			return nil, nil, safe(e)
		}
		if !exists {
			return nil, nil, f.ErrNotFound
		}
	}

	itemRows, e := tx.QueryContext(ctx, `SELECT i.id,i.kind,i.target_id,COALESCE(NULLIF(i.canonical_target_id,''),i.target_id),i.title,i.folder_id,fld.name,i.created_at FROM favorites.items i JOIN favorites.folders fld ON fld.owner_key=i.owner_key AND fld.id=i.folder_id WHERE i.owner_key=$1 AND NOT $7 AND ($2='' OR i.folder_id=$2) AND ($3='' OR i.kind=$3) AND ($4='' OR i.created_at<$5 OR (i.created_at=$5 AND i.id>$4)) AND $5::timestamptz IS NOT NULL ORDER BY i.created_at DESC,i.id ASC LIMIT $6`, owner, folderID, kind, pos.ID, pos.CreatedAt, limit, pos.ItemsDone)
	if e != nil {
		return nil, nil, safe(e)
	}
	items := []f.ExportItem{}
	for itemRows.Next() {
		var v f.ExportItem
		if err := itemRows.Scan(&v.ID, &v.Kind, &v.TargetID, &v.CanonicalTargetID, &v.Title, &v.FolderID, &v.FolderName, &v.CreatedAt); err != nil {
			itemRows.Close()
			return nil, nil, safe(err)
		}
		items = append(items, v)
	}
	e = itemRows.Err()
	itemRows.Close()
	if e != nil {
		return nil, nil, safe(e)
	}
	if e = tx.Commit(); e != nil {
		return nil, nil, safe(e)
	}
	return folders, items, nil
}
