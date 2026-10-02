package templates

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sharedcache "github.com/Bengo-Hub/cache"
	eventslib "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/notifications-api/internal/config"
)

var varRegex = regexp.MustCompile(`\{\{\s*(?:or\s+)?\.(\w+)`)

// Loader resolves template content: a platform edit stored in the database (Store) wins over
// the file baked into the image. Results are cached per pod in a bounded LRU with a TTL; an edit
// clears every pod's copy through the Broadcaster (SetInvalidator).
type Loader struct {
	cfg         config.TemplateConfig
	cache       *sharedcache.Local[string, string]
	store       Store
	invalidator *eventslib.Broadcaster
}

// Store persists platform template edits so every pod (API and worker) serves them and they
// survive redeploys. Keys are paths relative to the template root, e.g. "email/welcome.html".
// Before it existed, an edit was written to the serving pod's own filesystem: other pods kept
// the image version and the next deploy discarded it.
type Store interface {
	// Overrides returns the stored content for whichever of rels has an edit.
	Overrides(ctx context.Context, rels []string) (map[string]string, error)
	// SaveOverride stores content as the edit for rel (channel and name describe the template).
	SaveOverride(ctx context.Context, rel, channel, name, content string) error
}

const invalidateTopic = "template-changed"

func New(cfg config.TemplateConfig) *Loader {
	return &Loader{cfg: cfg, cache: sharedcache.NewLocal[string, string](2000, cfg.CacheTTL)}
}

// SetStore wires database-backed template edits. Without it edits go to the local filesystem
// (development only).
func (l *Loader) SetStore(st Store) { l.store = st }

// SetInvalidator makes an edit clear the cached template on every pod (API and worker).
func (l *Loader) SetInvalidator(b *eventslib.Broadcaster) {
	l.invalidator = b
	if b == nil {
		return
	}
	_ = b.Subscribe(invalidateTopic, func(m eventslib.BroadcastMessage) {
		l.cache.Delete(string(m.Data))
	})
}

func (l *Loader) invalidate(templateID string) {
	if l.invalidator == nil {
		l.cache.Delete(templateID)
		return
	}
	// Publish delivers to this pod's handler first, then to every other pod.
	_ = l.invalidator.Publish(invalidateTopic, "", "", []byte(templateID))
}

// Get loads the template content by identifier.
// templateID may be either "<channel>/<name>" or just "<name>" (then channel must be encoded in the ID by caller).
func (l *Loader) Get(ctx context.Context, templateID string) (string, error) {
	if content, ok := l.cache.Get(templateID); ok {
		return content, nil
	}

	// Load from filesystem. templateID arrives in API requests, so the resolved path must stay
	// inside the template directory: "../" segments would otherwise read any file in the
	// container into an outgoing message.
	baseDir, err := filepath.Abs(l.cfg.Directory)
	if err != nil {
		return "", fmt.Errorf("template dir: %w", err)
	}
	var path string
	if strings.Contains(templateID, "/") {
		path = filepath.Join(baseDir, templateID)
	} else {
		// default to email channel
		path = filepath.Join(baseDir, "email", templateID)
	}
	if !strings.HasPrefix(path, baseDir+string(filepath.Separator)) {
		return "", fmt.Errorf("template not found: %s", templateID)
	}
	candidates := []string{path, path + ".html", path + ".txt", path + ".mjml", path + ".json"}
	// A stored platform edit wins over the image file (one query for all candidate paths).
	if l.store != nil {
		rels := make([]string, 0, len(candidates))
		for _, c := range candidates {
			if rel, rerr := filepath.Rel(baseDir, c); rerr == nil {
				rels = append(rels, filepath.ToSlash(rel))
			}
		}
		if overrides, oerr := l.store.Overrides(ctx, rels); oerr == nil {
			for _, rel := range rels {
				if content, ok := overrides[rel]; ok {
					l.cache.Set(templateID, content)
					return content, nil
				}
			}
		}
	}
	// try with known extensions
	for _, p := range candidates {
		info, statErr := os.Stat(p)
		if statErr != nil || info.IsDir() {
			continue
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("read template: %w", err)
		}
		l.cache.Set(templateID, string(content))
		return string(content), nil
	}
	return "", fmt.Errorf("template not found: %s", templateID)
}

// Summary describes a template available for rendering.
type Summary struct {
	ID      string   `json:"id"`
	Channel string   `json:"channel"`
	Tags    []string `json:"tags"`
}

// Write saves template content to the filesystem under the configured directory.
// Only writes under baseDir; returns error if path would escape (e.g. path traversal).
// Channel must be one of: email, sms, push. Id must not contain path separators.
func (l *Loader) Write(ctx context.Context, channel, id, content string) error {
	if channel == "" || id == "" {
		return fmt.Errorf("channel and id required")
	}
	if strings.Contains(id, "/") || strings.Contains(id, "..") {
		return fmt.Errorf("invalid id: path traversal not allowed")
	}
	baseDir, err := filepath.Abs(l.cfg.Directory)
	if err != nil {
		return fmt.Errorf("template directory: %w", err)
	}
	channel = strings.ToLower(channel)
	switch channel {
	case "email", "sms", "push", "whatsapp":
	default:
		return fmt.Errorf("invalid channel: %s", channel)
	}
	ext := ".txt"
	if channel == "email" {
		ext = ".html"
	} else if channel == "push" {
		ext = ".json"
	}
	targetPath := filepath.Join(baseDir, channel, id+ext)
	// Ensure target is under baseDir
	rel, err := filepath.Rel(baseDir, targetPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("invalid path: write not under template directory")
	}
	if l.store != nil {
		if err := l.store.SaveOverride(ctx, filepath.ToSlash(rel), channel, id, content); err != nil {
			return fmt.Errorf("save template: %w", err)
		}
	} else {
		// Development without a database: write the file.
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}
		if err := os.WriteFile(targetPath, []byte(content), 0644); err != nil {
			return fmt.Errorf("write template: %w", err)
		}
	}
	l.invalidate(channel + "/" + id)
	return nil
}

// Directory returns the configured template base directory.
func (l *Loader) Directory() string { return l.cfg.Directory }

// ExtractVariables parses Go template content and returns unique variable names.
func ExtractVariables(content string) []string {
	seen := make(map[string]bool)
	var vars []string
	for _, m := range varRegex.FindAllStringSubmatch(content, -1) {
		if len(m) > 1 && !seen[m[1]] {
			seen[m[1]] = true
			vars = append(vars, m[1])
		}
	}
	return vars
}

// MimeTypeForExt returns the MIME type for a template file extension.
func MimeTypeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".html", ".mjml":
		return "text/html"
	case ".json":
		return "application/json"
	default:
		return "text/plain"
	}
}

// List scans the templates directory and returns available templates.
func (l *Loader) List(_ context.Context) ([]Summary, error) {
	var out []Summary
	base := l.cfg.Directory
	channels := []string{"email", "sms", "push", "whatsapp"}
	exts := map[string]bool{".html": true, ".txt": true, ".mjml": true, ".json": true}

	for _, ch := range channels {
		dir := filepath.Join(base, ch)
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !exts[strings.ToLower(filepath.Ext(d.Name()))] {
				return nil
			}

			// Get relative path from channel directory
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return nil
			}

			// Use forward slashes for IDs regardless of OS
			id := strings.TrimSuffix(rel, filepath.Ext(rel))
			id = filepath.ToSlash(id)

			// Extract tags from subdirectory path segments
			var tags []string
			parts := strings.Split(id, "/")
			if len(parts) > 1 {
				tags = parts[:len(parts)-1]
			}

			out = append(out, Summary{ID: id, Channel: ch, Tags: tags})
			return nil
		})
	}
	return out, nil
}
