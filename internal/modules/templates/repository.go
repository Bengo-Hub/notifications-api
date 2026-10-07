package templates

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/Bengo-Hub/cache"
	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/notifications-api/internal/ent"
	enttemplate "github.com/bengobox/notifications-api/internal/ent/template"
	"github.com/google/uuid"
)

// TTLTemplates is the cache duration for template list queries.
const TTLTemplates = 2 * time.Hour

// Filters holds optional query filters for template listing.
type Filters struct {
	Channel  string
	Category string
	Tag      string
	Search   string
}

// TemplateSummary is the API-facing template metadata.
type TemplateSummary struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Channel     string    `json:"channel"`
	Category    string    `json:"category"`
	Tags        []string  `json:"tags"`
	Variables   []string  `json:"variables"`
	MimeType    string    `json:"mimeType"`
	Description string    `json:"description,omitempty"`
	FilePath    string    `json:"filePath"`
}

// CachedListResult holds a paginated template list for cache serialization.
type CachedListResult struct {
	Data  []TemplateSummary `json:"data"`
	Total int               `json:"total"`
}

// Repository provides DB access for template metadata.
type Repository struct {
	client *ent.Client
	cache  *cache.Aside
}

// NewRepository creates a template repository.
func NewRepository(client *ent.Client, c *cache.Aside) *Repository {
	return &Repository{client: client, cache: c}
}

// List queries templates with pagination, filtering, and caching.
func (r *Repository) List(ctx context.Context, p pagination.Params, f Filters) ([]TemplateSummary, int, error) {
	cacheKey := cache.Key("notif", "templates", f.Channel, f.Category, f.Tag, f.Search, cache.FormatPage(p.Page, p.Limit))

	result, err := cache.GetOrSet(ctx, r.cache, cacheKey, TTLTemplates, func(ctx context.Context) (CachedListResult, error) {
		q := r.client.Template.Query().Where(enttemplate.IsActive(true))

		if f.Channel != "" {
			q = q.Where(enttemplate.Channel(f.Channel))
		}
		if f.Category != "" {
			q = q.Where(enttemplate.Category(f.Category))
		}
		if f.Search != "" {
			q = q.Where(enttemplate.NameContainsFold(f.Search))
		}
		if f.Tag != "" {
			// Tag is a query-string value: matched as a bound parameter against the JSON array,
			// never formatted into the SQL text.
			q = q.Where(func(s *sql.Selector) {
				s.Where(sqljson.ValueContains(enttemplate.FieldTags, f.Tag))
			})
		}

		total, err := q.Clone().Count(ctx)
		if err != nil {
			return CachedListResult{}, fmt.Errorf("count templates: %w", err)
		}

		rows, err := q.
			Order(ent.Asc(enttemplate.FieldChannel), ent.Asc(enttemplate.FieldCategory), ent.Asc(enttemplate.FieldName)).
			Offset(p.Offset).
			Limit(p.Limit).
			All(ctx)
		if err != nil {
			return CachedListResult{}, fmt.Errorf("query templates: %w", err)
		}

		data := make([]TemplateSummary, 0, len(rows))
		for _, t := range rows {
			tags := t.Tags
			if tags == nil {
				tags = []string{}
			}
			vars := t.Variables
			if vars == nil {
				vars = []string{}
			}
			data = append(data, TemplateSummary{
				ID:          t.ID,
				Name:        t.Name,
				Channel:     t.Channel,
				Category:    t.Category,
				Tags:        tags,
				Variables:   vars,
				MimeType:    t.MimeType,
				Description: t.Description,
				FilePath:    t.FilePath,
			})
		}

		return CachedListResult{Data: data, Total: total}, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return result.Data, result.Total, nil
}

// GetByID fetches a single template by UUID.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*ent.Template, error) {
	return r.client.Template.Get(ctx, id)
}

// GetByChannelAndName fetches a template by channel + name.
func (r *Repository) GetByChannelAndName(ctx context.Context, channel, name string) (*ent.Template, error) {
	return r.client.Template.Query().
		Where(enttemplate.Channel(channel), enttemplate.Name(name), enttemplate.IsActive(true)).
		Only(ctx)
}

// InvalidateCache clears all template list caches.
func (r *Repository) InvalidateCache(ctx context.Context) {
	if r.cache != nil {
		r.cache.InvalidatePattern(ctx, "notif:templates:*")
	}
}

// MarshalJSON helper — needed so CachedListResult round-trips through Redis.
func (c CachedListResult) MarshalBinary() ([]byte, error) {
	return json.Marshal(c)
}
