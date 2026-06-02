package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// itemColumns is the canonical SELECT list shared by every item read in this
// package, kept in one place so scanItem and the queries can't drift.
const itemColumns = `id, type, title, sorttitle, year, rating, description,
	tagline, durationms, seasonnumber, episodenumber, parent_id`

// ListItems returns a page of catalog items for the management library view,
// plus the unfiltered match count for the same filter. Title search is a
// case-insensitive contains-match (ILIKE) — the management UI does not need
// the read API's full-text ranking.
func (s *Store) ListItems(ctx context.Context, opts ListOpts) (ListResult, error) {
	if s == nil || s.Pool == nil {
		return ListResult{}, ErrNoPool
	}
	if opts.Limit <= 0 || opts.Limit > 200 {
		opts.Limit = 50
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}

	var (
		args   []any
		wheres []string
	)
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if opts.Type != "" {
		wheres = append(wheres, "type = "+add(opts.Type))
	}
	if opts.Query != "" {
		wheres = append(wheres, "title ILIKE "+add("%"+opts.Query+"%"))
	}

	where := ""
	if len(wheres) > 0 {
		where = "WHERE " + strings.Join(wheres, " AND ")
	}

	var total int
	if err := s.Pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM com_nalet_katalog_items "+where, args...).Scan(&total); err != nil {
		return ListResult{}, fmt.Errorf("count items: %w", err)
	}

	listSQL := "SELECT " + itemColumns + " FROM com_nalet_katalog_items " + where +
		" ORDER BY sorttitle ASC NULLS LAST, id ASC LIMIT " + add(opts.Limit) +
		" OFFSET " + add(opts.Offset)

	rows, err := s.Pool.Query(ctx, listSQL, args...)
	if err != nil {
		return ListResult{}, fmt.Errorf("list items: %w", err)
	}
	defer rows.Close()

	items := make([]Item, 0, opts.Limit)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return ListResult{}, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, err
	}
	return ListResult{Items: items, Total: total, Limit: opts.Limit, Offset: opts.Offset}, nil
}

// GetItem returns a single item by id. ErrNotFound when it doesn't exist.
func (s *Store) GetItem(ctx context.Context, id string) (Item, error) {
	if s == nil || s.Pool == nil {
		return Item{}, ErrNoPool
	}
	row := s.Pool.QueryRow(ctx,
		"SELECT "+itemColumns+" FROM com_nalet_katalog_items WHERE id = $1", id)
	it, err := scanItem(row)
	if isNoRows(err) {
		return Item{}, ErrNotFound
	}
	return it, err
}

// UpdateItem applies a partial patch to an item's editable fields. Only the
// non-nil fields of u are written, built dynamically so a single-field edit
// doesn't clobber the rest. ErrNotFound when the id doesn't exist.
func (s *Store) UpdateItem(ctx context.Context, id string, u ItemUpdate) (Item, error) {
	if s == nil || s.Pool == nil {
		return Item{}, ErrNoPool
	}
	var (
		sets []string
		args []any
	)
	set := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if u.Title != nil {
		set("title", *u.Title)
	}
	if u.SortTitle != nil {
		set("sorttitle", *u.SortTitle)
	}
	if u.Year != nil {
		set("year", *u.Year)
	}
	if u.Rating != nil {
		set("rating", *u.Rating)
	}
	if u.Description != nil {
		set("description", *u.Description)
	}
	if u.Tagline != nil {
		set("tagline", *u.Tagline)
	}
	if len(sets) == 0 {
		// Nothing to change — return the current row so the caller still
		// gets a well-formed response.
		return s.GetItem(ctx, id)
	}
	sets = append(sets, "modifiedat = now()")
	args = append(args, id)

	sql := "UPDATE com_nalet_katalog_items SET " + strings.Join(sets, ", ") +
		fmt.Sprintf(" WHERE id = $%d RETURNING %s", len(args), itemColumns)

	row := s.Pool.QueryRow(ctx, sql, args...)
	it, err := scanItem(row)
	if isNoRows(err) {
		return Item{}, ErrNotFound
	}
	return it, err
}

// DeleteItem removes an item and (via ON DELETE CASCADE on the composition
// tables) its associated rows. ErrNotFound when the id doesn't exist.
func (s *Store) DeleteItem(ctx context.Context, id string) error {
	if s == nil || s.Pool == nil {
		return ErrNoPool
	}
	tag, err := s.Pool.Exec(ctx,
		"DELETE FROM com_nalet_katalog_items WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RegisteredAsset is the minimal record the import scanner writes per file:
// an item row plus its primary playback-asset row pointing at the on-disk
// path. The operator owns the file; this only records that it exists.
type RegisteredAsset struct {
	ItemID string
	Path   string
}

// AssetPathExists reports whether a playback asset already points at path.
// The scanner uses this to skip files already in the catalog so re-scanning a
// directory is idempotent.
func (s *Store) AssetPathExists(ctx context.Context, path string) (bool, error) {
	if s == nil || s.Pool == nil {
		return false, ErrNoPool
	}
	var exists bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM com_nalet_katalog_playbackassets WHERE path = $1
		)`, path).Scan(&exists)
	return exists, err
}

// RegisterItem inserts one catalog item + its primary playback asset in a
// single transaction. title and itemType come from the scanner's filename
// parse; path is the on-disk location of the file the operator owns. Returns
// the new item id.
//
// This is the neutral replacement for the previous acquisition-coupled write
// path: it only records files that already exist on disk. It never fetches,
// downloads, or requests anything.
func (s *Store) RegisterItem(ctx context.Context, itemType, title, path string) (string, error) {
	if s == nil || s.Pool == nil {
		return "", ErrNoPool
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	itemID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO com_nalet_katalog_items
			(id, type, title, sorttitle, createdat, modifiedat)
		VALUES ($1, $2, $3, $3, now(), now())`,
		itemID, itemType, title); err != nil {
		return "", fmt.Errorf("insert item: %w", err)
	}

	assetID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO com_nalet_katalog_playbackassets
			(id, item_id, path, isprimary, kind)
		VALUES ($1, $2, $3, true, 'primary')`,
		assetID, itemID, path); err != nil {
		return "", fmt.Errorf("insert asset: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit tx: %w", err)
	}
	return itemID, nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanItem reads the canonical item column set into an Item, COALESCE-ing the
// nullable text columns into "" so the JSON omits them rather than carrying
// null. Year / rating / season / episode stay pointers because null is
// meaningful there.
func scanItem(r rowScanner) (Item, error) {
	var (
		it          Item
		sortTitle   *string
		year        *int
		rating      *float64
		description *string
		tagline     *string
		durationMs  *int64
		seasonNum   *int
		episodeNum  *int
		parentID    *string
	)
	if err := r.Scan(
		&it.ID, &it.Type, &it.Title,
		&sortTitle, &year, &rating,
		&description, &tagline, &durationMs,
		&seasonNum, &episodeNum, &parentID,
	); err != nil {
		return Item{}, err
	}
	if sortTitle != nil {
		it.SortTitle = *sortTitle
	}
	it.Year = year
	if rating != nil {
		it.Rating = *rating
	}
	if description != nil {
		it.Description = *description
	}
	if tagline != nil {
		it.Tagline = *tagline
	}
	if durationMs != nil {
		it.DurationMs = *durationMs
	}
	it.SeasonNumber = seasonNum
	it.EpisodeNumber = episodeNum
	if parentID != nil {
		it.ParentID = *parentID
	}
	return it, nil
}
