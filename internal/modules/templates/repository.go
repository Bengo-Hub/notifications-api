package templates

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Bengo-Hub/cache"
	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/notifications-api/internal/ent"
	enttemplate "github.com/bengobox/notifications-api/internal/ent/template"
	"github.com/google/uuid"
	"entgo.io/ent/dialect/sql"
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
	fmt.Printf("[DEBUG] List called with page=%d, limit=%d, filters=%+v\n", p.Page, p.Limit, f)
	cacheKey := cache.Key("notif", "templates", f.Channel, f.Category, f.Tag, f.Search, cache.FormatPage(p.Page, p.Limit))
	fmt.Printf("[DEBUG] Cache key: %s\n", cacheKey)

	result, err := cache.GetOrSet(ctx, r.cache, cacheKey, TTLTemplates, func(ctx context.Context) (CachedListResult, error) {
		fmt.Printf("[DEBUG] Cache miss or error - executing fetch callback\n")
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
			// Search in tags JSON array
			q = q.Where(func(s *sql.Selector) {
				s.Where(sql.ExprP(fmt.Sprintf("tags ?? '%s'", f.Tag)))
			})
		}

		total, err := q.Clone().Count(ctx)
		fmt.Printf("[DEBUG] Templates count query result: total=%d, err=%v, filters=%+v\n", total, err, f)
		if err != nil {
			fmt.Printf("[DEBUG] ERROR in count: %v\n", err)
			return CachedListResult{}, fmt.Errorf("count templates: %w", err)
		}

		fmt.Printf("[DEBUG] About to query rows with offset=%d, limit=%d\n", p.Offset, p.Limit)

		rows, err := q.
			Order(ent.Asc(enttemplate.FieldChannel), ent.Asc(enttemplate.FieldCategory), ent.Asc(enttemplate.FieldName)).
			Offset(p.Offset).
			Limit(p.Limit).
			All(ctx)
		fmt.Printf("[DEBUG] Rows query result: count=%d, err=%v\n", len(rows), err)
		if err != nil {
			fmt.Printf("[DEBUG] ERROR in All(): %v\n", err)
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
		fmt.Printf("[DEBUG] GetOrSet returned error: %v\n", err)
		return nil, 0, err
	}
	fmt.Printf("[DEBUG] GetOrSet returned: totalCount=%d, dataLen=%d\n", result.Total, len(result.Data))
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
