// Package repository is the persistence layer: it wraps the ent client and
// translates ent results and errors into model types.
package repository

import (
	"context"
	"fmt"
	"time"

	"url_shortener/ent"
	"url_shortener/ent/link"
	"url_shortener/internal/model"
)

type Links struct {
	client *ent.Client
}

func NewLinks(client *ent.Client) *Links {
	return &Links{client: client}
}

// NextID reserves the next value of the links id sequence, so a short code can
// be derived from the id before the row is inserted.
func (r *Links) NextID(ctx context.Context) (int64, error) {
	rows, err := r.client.QueryContext(ctx, `SELECT nextval(pg_get_serial_sequence('links', 'id'))`)
	if err != nil {
		return 0, fmt.Errorf("reserve link id: %w", err)
	}
	defer rows.Close()

	var id int64
	if !rows.Next() {
		return 0, fmt.Errorf("reserve link id: %w", rows.Err())
	}
	if err := rows.Scan(&id); err != nil {
		return 0, fmt.Errorf("reserve link id: %w", err)
	}
	return id, nil
}

func (r *Links) Create(ctx context.Context, l model.NewLink) (model.Link, error) {
	created, err := r.client.Link.Create().
		SetID(l.ID).
		SetShortCode(l.ShortCode).
		SetOriginalURL(l.OriginalURL).
		SetNillableExpiresAt(l.ExpiresAt).
		Save(ctx)
	if ent.IsConstraintError(err) {
		return model.Link{}, model.ErrShortCodeTaken
	}
	if err != nil {
		return model.Link{}, fmt.Errorf("create link: %w", err)
	}
	return toModel(created), nil
}

func (r *Links) FindByShortCode(ctx context.Context, code string) (model.Link, error) {
	found, err := r.client.Link.Query().
		Where(link.ShortCode(code)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return model.Link{}, model.ErrNotFound
	}
	if err != nil {
		return model.Link{}, fmt.Errorf("find link by short code: %w", err)
	}
	return toModel(found), nil
}

// FindPermanentByURL returns the oldest link for originalURL that never
// expires. Links with an expiry are excluded so a caller asking for a
// permanent link is never handed one that will stop working.
func (r *Links) FindPermanentByURL(ctx context.Context, originalURL string) (model.Link, error) {
	found, err := r.client.Link.Query().
		Where(link.OriginalURL(originalURL), link.ExpiresAtIsNil()).
		Order(ent.Asc(link.FieldID)).
		First(ctx)
	if ent.IsNotFound(err) {
		return model.Link{}, model.ErrNotFound
	}
	if err != nil {
		return model.Link{}, fmt.Errorf("find link by url: %w", err)
	}
	return toModel(found), nil
}

func (r *Links) Delete(ctx context.Context, code string) error {
	deleted, err := r.client.Link.Delete().
		Where(link.ShortCode(code)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	if deleted == 0 {
		return model.ErrNotFound
	}
	return nil
}

// AddClicks applies every count in a single statement: the codes and deltas
// are sent as two parallel arrays and joined back together with unnest.
func (r *Links) AddClicks(ctx context.Context, counts map[string]int64) error {
	codes := make([]string, 0, len(counts))
	deltas := make([]int64, 0, len(counts))
	for code, delta := range counts {
		codes = append(codes, code)
		deltas = append(deltas, delta)
	}

	_, err := r.client.ExecContext(ctx, `
		UPDATE links
		SET click_count = links.click_count + batch.delta
		FROM unnest($1::text[], $2::bigint[]) AS batch(short_code, delta)
		WHERE links.short_code = batch.short_code`,
		codes, deltas,
	)
	if err != nil {
		return fmt.Errorf("add clicks: %w", err)
	}
	return nil
}

func (r *Links) MarkExpired(ctx context.Context, now time.Time) (int, error) {
	marked, err := r.client.Link.Update().
		Where(link.ExpiresAtLT(now), link.Expired(false)).
		SetExpired(true).
		Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("mark expired links: %w", err)
	}
	return marked, nil
}

// toModel also normalises timestamps to UTC; pgx returns them in the
// process's local time zone, which would make API output vary by host.
func toModel(l *ent.Link) model.Link {
	var expiresAt *time.Time
	if l.ExpiresAt != nil {
		utc := l.ExpiresAt.UTC()
		expiresAt = &utc
	}

	return model.Link{
		ID:          l.ID,
		ShortCode:   l.ShortCode,
		OriginalURL: l.OriginalURL,
		ClickCount:  l.ClickCount,
		CreatedAt:   l.CreatedAt.UTC(),
		ExpiresAt:   expiresAt,
		Expired:     l.Expired,
	}
}
