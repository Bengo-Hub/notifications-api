package templates

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sharedcache "github.com/Bengo-Hub/cache"

	"github.com/bengobox/notifications-api/internal/config"
)

var varRegex = regexp.MustCompile(`\{\{\s*(?:or\s+)?\.(\w+)`)

// Loader caches template files in memory with a TTL. The cache is bounded (least recently
// used templates are dropped first) so it cannot grow with arbitrary template IDs.
type Loader struct {
	cfg   config.TemplateConfig
	cache *sharedcache.Local[string, string]
}

func New(cfg config.TemplateConfig) *Loader {
	return &Loader{cfg: cfg, cache: sharedcache.NewLocal[string, string](2000, cfg.CacheTTL)}
}

// Get loads the template content by identifier.
// templateID may be either "<channel>/<name>" or just "<name>" (then channel must be encoded in the ID by caller).
func (l *Loader) Get(_ context.Context, templateID string) (string, error) {
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
	// try with known extensions
	for _, p := range []string{path, path + ".html", path + ".txt", path + ".mjml", path + ".json"} {
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
func (l *Loader) Write(_ context.Context, channel, id, content string) error {
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
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(targetPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write template: %w", err)
	}
	// Invalidate this pod's cached copy. NOTE: the file itself is pod-local (see the queued
	// template-persistence item in the multi-pod plan); other replicas keep the image version.
	l.cache.Delete(channel + "/" + id)
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
