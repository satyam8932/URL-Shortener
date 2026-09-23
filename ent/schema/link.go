// Package schema defines the ent schemas that the database tables and the
// generated client are derived from.
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ShortCodeMaxLen bounds the length of a short code. Base62 needs at most 11
// characters for any int64, but 10 characters already cover ~8.4e17 IDs,
// which is far beyond anything this service will issue.
const ShortCodeMaxLen = 10

// Link is a shortened URL: the mapping from a public short code to its
// original destination, plus the click counter and lifecycle metadata.
//
// The id column is a Postgres identity sequence and is the source that short
// codes are derived from (see ARCHITECTURE.md §5). short_code is kept as a
// separate column so the encoding scheme can change without touching the
// primary key.
type Link struct {
	ent.Schema
}

// Fields of the Link.
func (Link) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id").
			Immutable(),

		field.String("short_code").
			NotEmpty().
			MaxLen(ShortCodeMaxLen).
			Unique().
			Immutable().
			SchemaType(map[string]string{dialect.Postgres: "varchar(10)"}),

		field.Text("original_url").
			NotEmpty().
			Immutable(),

		field.Int64("click_count").
			Default(0).
			NonNegative(),

		field.Time("created_at").
			Default(time.Now).
			Immutable().
			Annotations(entsql.Default("CURRENT_TIMESTAMP")),

		field.Time("expires_at").
			Optional().
			Nillable(),

		// Set by the expiry sweeper so the redirect path never compares timestamps.
		field.Bool("expired").
			Default(false),
	}
}

// Indexes of the Link.
//
// original_url uses a HASH index rather than the default B-tree: the
// idempotency lookup is equality-only, and B-tree entries are capped at
// roughly 2.7KB, which a long URL can exceed and turn into a failed insert.
//
// expires_at is indexed partially so the expiry sweeper scans only links that
// can actually expire.
func (Link) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("original_url").
			Annotations(entsql.IndexType("HASH")),
		index.Fields("expires_at").
			Annotations(entsql.IndexWhere("expires_at IS NOT NULL")),
	}
}
