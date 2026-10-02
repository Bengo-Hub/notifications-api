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

type fakeStore struct{ edits map[string]string }

func (f *fakeStore) Overrides(_ context.Context, rels []string) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range rels {
		if c, ok := f.edits[r]; ok {
			out[r] = c
		}
	}
	return out, nil
}

func (f *fakeStore) SaveOverride(_ context.Context, rel, _, _, content string) error {
	f.edits[rel] = content
	return nil
}

// An edit is stored (not written to the pod's filesystem), wins over the image file, is visible
// immediately, and works for a template that only exists as an edit.
func TestLoaderStoredEditsWinOverFiles(t *testing.T) {
	root := t.TempDir()
	tplDir := filepath.Join(root, "templates")
	_ = os.MkdirAll(filepath.Join(tplDir, "email"), 0o755)
	_ = os.WriteFile(filepath.Join(tplDir, "email", "welcome.html"), []byte("from image"), 0o644)
	st := &fakeStore{edits: map[string]string{}}
	l := New(config.TemplateConfig{Directory: tplDir, CacheTTL: time.Minute})
	l.SetStore(st)
	ctx := context.Background()

	if got, _ := l.Get(ctx, "email/welcome"); got != "from image" {
		t.Fatalf("before edit got %q", got)
	}
	if err := l.Write(ctx, "email", "welcome", "edited"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(tplDir, "email", "welcome.html")); string(b) != "from image" {
		t.Fatal("an edit must not be written to the pod filesystem when a store is set")
	}
	if got, _ := l.Get(ctx, "email/welcome"); got != "edited" {
		t.Fatalf("after edit got %q", got)
	}
	_ = l.Write(ctx, "sms", "brand_new", "only in db")
	if got, err := l.Get(ctx, "sms/brand_new"); err != nil || got != "only in db" {
		t.Fatalf("edit-only template: %q %v", got, err)
	}
}
