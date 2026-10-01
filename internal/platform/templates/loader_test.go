package templates

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bengobox/notifications-api/internal/config"
)

func TestLoaderRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	tplDir := filepath.Join(root, "templates")
	_ = os.MkdirAll(filepath.Join(tplDir, "email"), 0o755)
	_ = os.WriteFile(filepath.Join(tplDir, "email", "welcome.html"), []byte("hi"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "secret.txt"), []byte("SECRET"), 0o644)

	l := New(config.TemplateConfig{Directory: tplDir, CacheTTL: time.Minute})
	if got, err := l.Get(context.Background(), "email/welcome"); err != nil || got != "hi" {
		t.Fatalf("valid template: %q %v", got, err)
	}
	for _, id := range []string{"../secret", "email/../../secret", "../secret.txt"} {
		if got, err := l.Get(context.Background(), id); err == nil {
			t.Fatalf("%s escaped the template dir and read %q", id, got)
		}
	}
}
