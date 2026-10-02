package templates

import (
	"context"
	"path"
	"strings"

	"github.com/bengobox/notifications-api/internal/ent"
	enttemplate "github.com/bengobox/notifications-api/internal/ent/template"
)

// OverrideStore keeps platform template edits in the templates table (content_override), keyed
// by the file path relative to the template root. It implements platform/templates.Store, so
// the API and the worker on every pod read the same edit, and edits survive redeploys.
type OverrideStore struct {
	client *ent.Client
}

// NewOverrideStore creates the store.
func NewOverrideStore(client *ent.Client) *OverrideStore {
	return &OverrideStore{client: client}
}

// Overrides returns the stored edit for each of rels that has one.
func (s *OverrideStore) Overrides(ctx context.Context, rels []string) (map[string]string, error) {
	out := map[string]string{}
	if len(rels) == 0 {
		return out, nil
	}
	rows, err := s.client.Template.Query().
		Where(enttemplate.FilePathIn(rels...), enttemplate.ContentOverrideNotNil()).
		Select(enttemplate.FieldFilePath, enttemplate.FieldContentOverride).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.ContentOverride != nil {
			out[r.FilePath] = *r.ContentOverride
		}
	}
	return out, nil
}

// SaveOverride stores content as the edit for rel, updating the template's row or creating one
// for a template that only exists as an edit.
func (s *OverrideStore) SaveOverride(ctx context.Context, rel, channel, name, content string) error {
	n, err := s.client.Template.Update().
		Where(enttemplate.FilePath(rel)).
		SetContentOverride(content).
		Save(ctx)
	if err != nil || n > 0 {
		return err
	}
	mime := "text/plain"
	switch strings.ToLower(path.Ext(rel)) {
	case ".html":
		mime = "text/html"
	case ".json":
		mime = "application/json"
	}
	return s.client.Template.Create().
		SetName(name).
		SetChannel(channel).
		SetCategory("custom").
		SetFilePath(rel).
		SetMimeType(mime).
		SetContentOverride(content).
		Exec(ctx)
}
