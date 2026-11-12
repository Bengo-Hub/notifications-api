package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// fakeMeta answers the Graph calls the profile service makes, like Meta does.
type fakeMeta struct {
	mu        sync.Mutex
	calls     []string
	auth      map[string]string
	headers   map[string]http.Header
	bodies    map[string][]byte
	queries   map[string]string
	profile   string // JSON of the whatsapp_business_profile data[0] object
	phone     string // JSON of GET /{phone-number-id}
	failPath  string
	failBody  string
	failCode  int
	uploadIDs int
}

func newFakeMeta(t *testing.T) (*fakeMeta, *httptest.Server) {
	t.Helper()
	g := &fakeMeta{
		auth: map[string]string{}, headers: map[string]http.Header{}, bodies: map[string][]byte{}, queries: map[string]string{},
		profile: `{"about":"","address":"","description":"","email":"","websites":[],"vertical":"","messaging_product":"whatsapp"}`,
		phone:   `{"verified_name":"Hadia","name_status":"APPROVED","display_phone_number":"+254 700 000000","quality_rating":"GREEN","id":"P1"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		g.calls = append(g.calls, key)
		g.auth[key] = r.Header.Get("Authorization")
		g.headers[key] = r.Header.Clone()
		g.bodies[key] = b
		g.queries[key] = r.URL.RawQuery
		if r.URL.Path == g.failPath {
			code := g.failCode
			if code == 0 {
				code = http.StatusBadRequest
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(g.failBody))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/uploads"):
			g.uploadIDs++
			_, _ = w.Write([]byte(`{"id":"upload:SESSION1?sig=abc"}`))
		case strings.Contains(r.URL.Path, "/upload:"):
			_, _ = w.Write([]byte(`{"h":"HANDLE-1"}`))
		case strings.HasSuffix(r.URL.Path, "/whatsapp_business_profile") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[` + g.profile + `]}`))
		case strings.HasSuffix(r.URL.Path, "/whatsapp_business_profile"):
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			_, _ = w.Write([]byte(g.phone))
		}
	}))
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *fakeMeta) body(key string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(g.bodies[key], &m)
	return m
}

func (g *fakeMeta) called(prefix string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// newTestService wires the service to the fake Graph server. Each tenant has its own number and
// token; the branding map gives each tenant's branding.
func newTestService(t *testing.T, base string, branding map[string]Branding) (*Service, *[]string) {
	t.Helper()
	var mu sync.Mutex
	asked := &[]string{}
	creds := func(_ context.Context, tenantID string) (Credentials, error) {
		mu.Lock()
		*asked = append(*asked, tenantID)
		mu.Unlock()
		if tenantID == "no-number" {
			return Credentials{}, errors.New("no WhatsApp number is connected")
		}
		return Credentials{PhoneNumberID: "P-" + tenantID, AccessToken: "tok-" + tenantID, APIVersion: "v26.0"}, nil
	}
	appID := func(context.Context) (string, error) { return "APP1", nil }
	brand := func(_ context.Context, tenantID string) (Branding, error) {
		b, ok := branding[tenantID]
		if !ok {
			return Branding{}, errors.New("no branding")
		}
		return b, nil
	}
	s := New(creds, appID, brand, zap.NewNop())
	s.GraphURL = base
	s.ImageClient = http.DefaultClient // the fake logo host is on loopback
	return s, asked
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{R: 200, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGetReadsProfileAndDisplayName(t *testing.T) {
	g, srv := newFakeMeta(t)
	g.profile = `{"about":"Fresh daily","address":"Moi Ave","description":"Bakery","email":"hi@example.com","websites":["https://example.com"],"vertical":"RESTAURANT","profile_picture_url":"https://pps.example/p.jpg","messaging_product":"whatsapp"}`
	g.phone = `{"verified_name":"Hadia","name_status":"APPROVED","new_display_name":"Hadia Foods","new_name_status":"PENDING_REVIEW","display_phone_number":"+254 700 000000","quality_rating":"GREEN","id":"P-t1"}`
	s, _ := newTestService(t, srv.URL, nil)

	p, err := s.Get(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if p.DisplayName != "Hadia" || p.NameStatus != "APPROVED" || p.NewDisplayName != "Hadia Foods" || p.NewNameStatus != "PENDING_REVIEW" {
		t.Errorf("name fields = %+v", p)
	}
	if p.QualityRating != "GREEN" || p.DisplayPhoneNumber != "+254 700 000000" || p.PhoneNumberID != "P-t1" {
		t.Errorf("number fields = %+v", p)
	}
	if p.About != "Fresh daily" || p.Address != "Moi Ave" || p.Description != "Bakery" || p.Email != "hi@example.com" ||
		len(p.Websites) != 1 || p.Vertical != "RESTAURANT" || p.PhotoURL != "https://pps.example/p.jpg" {
		t.Errorf("profile fields = %+v", p)
	}
	phoneKey := "GET /v26.0/P-t1"
	for _, f := range []string{"verified_name", "name_status", "new_name_status", "display_phone_number", "quality_rating"} {
		if !strings.Contains(g.queries[phoneKey], f) {
			t.Errorf("phone number query %q misses %s", g.queries[phoneKey], f)
		}
	}
	profKey := "GET /v26.0/P-t1/whatsapp_business_profile"
	for _, f := range []string{"about", "address", "description", "email", "websites", "vertical", "profile_picture_url"} {
		if !strings.Contains(g.queries[profKey], f) {
			t.Errorf("profile query %q misses %s", g.queries[profKey], f)
		}
	}
	if g.auth[phoneKey] != "Bearer tok-t1" || g.auth[profKey] != "Bearer tok-t1" {
		t.Errorf("auth = %q / %q, want the tenant's own token", g.auth[phoneKey], g.auth[profKey])
	}
}

func TestUpdateSendsOnlyGivenFields(t *testing.T) {
	g, srv := newFakeMeta(t)
	s, _ := newTestService(t, srv.URL, nil)
	about, email := "We bake", "shop@example.com"
	sites := []string{"example.com", "https://shop.example.com"}
	if err := s.Update(context.Background(), "t1", Update{About: &about, Email: &email, Websites: &sites}); err != nil {
		t.Fatal(err)
	}
	b := g.body("POST /v26.0/P-t1/whatsapp_business_profile")
	if b["messaging_product"] != "whatsapp" || b["about"] != "We bake" || b["email"] != "shop@example.com" {
		t.Fatalf("body = %v", b)
	}
	ws, _ := b["websites"].([]any)
	if len(ws) != 2 || ws[0] != "https://example.com" {
		t.Errorf("websites = %v, want a scheme added", ws)
	}
	for _, k := range []string{"address", "description", "vertical", "profile_picture_handle"} {
		if _, ok := b[k]; ok {
			t.Errorf("%s sent although not given", k)
		}
	}
}

func TestUpdateValidatesMetaLimits(t *testing.T) {
	_, srv := newFakeMeta(t)
	s, _ := newTestService(t, srv.URL, nil)
	long := strings.Repeat("a", 140)
	empty := ""
	badEmail := "not-an-email"
	threeSites := []string{"https://a.com", "https://b.com", "https://c.com"}
	badVertical := "BAKERY"
	longDesc := strings.Repeat("d", 513)
	longAddr := strings.Repeat("x", 257)
	for name, u := range map[string]Update{
		"about too long":   {About: &long},
		"about empty":      {About: &empty},
		"bad email":        {Email: &badEmail},
		"three websites":   {Websites: &threeSites},
		"unknown vertical": {Vertical: &badVertical},
		"long description": {Description: &longDesc},
		"long address":     {Address: &longAddr},
	} {
		if err := s.Update(context.Background(), "t1", u); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestGraphErrorPassesThrough(t *testing.T) {
	g, srv := newFakeMeta(t)
	g.failPath = "/v26.0/P-t1/whatsapp_business_profile"
	g.failBody = `{"error":{"message":"(#100) Param email must be a valid email","type":"OAuthException","code":100,"error_user_msg":"Enter a valid email","fbtrace_id":"X"}}`
	s, _ := newTestService(t, srv.URL, nil)
	email := "a@b.co"
	err := s.Update(context.Background(), "t1", Update{Email: &email})
	var ge *GraphError
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v, want a GraphError", err)
	}
	if ge.Code != 100 || ge.Status != http.StatusBadRequest || !strings.Contains(ge.Error(), "valid email") {
		t.Errorf("graph error = %+v (%s)", ge, ge.Error())
	}
	if strings.Contains(ge.Error(), "tok-t1") {
		t.Error("error leaks the access token")
	}
}

func TestCredentialErrorsAreClientErrors(t *testing.T) {
	s, _ := newTestService(t, "http://unused", nil)
	if _, err := s.Get(context.Background(), "no-number"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("err = %v, want ErrCredentials", err)
	}
}

// The photo goes up in two steps (open a session on the app, send the bytes with file_offset 0
// and an OAuth header), then the handle is set on the profile.
func TestSetPhotoUploadsInTwoStepsAndSetsHandle(t *testing.T) {
	g, srv := newFakeMeta(t)
	s, _ := newTestService(t, srv.URL, nil)
	img := testPNG(t, 640, 640)

	if err := s.SetPhotoBytes(context.Background(), "t1", img); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /v26.0/APP1/uploads", "POST /v26.0/upload:SESSION1", "POST /v26.0/P-t1/whatsapp_business_profile"}
	if strings.Join(g.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", g.calls, want)
	}
	q := g.queries["POST /v26.0/APP1/uploads"]
	for _, part := range []string{"file_length=" + itoa(len(img)), "file_type=image%2Fpng"} {
		if !strings.Contains(q, part) {
			t.Errorf("session query %q misses %s", q, part)
		}
	}
	upKey := "POST /v26.0/upload:SESSION1"
	if g.queries[upKey] != "sig=abc" {
		t.Errorf("upload query = %q, want the session's own sig kept", g.queries[upKey])
	}
	if g.auth[upKey] != "OAuth tok-t1" || g.headers[upKey].Get("file_offset") != "0" {
		t.Errorf("upload headers: auth %q file_offset %q", g.auth[upKey], g.headers[upKey].Get("file_offset"))
	}
	if !bytes.Equal(g.bodies[upKey], img) {
		t.Error("upload body is not the image bytes")
	}
	if b := g.body("POST /v26.0/P-t1/whatsapp_business_profile"); b["profile_picture_handle"] != "HANDLE-1" || b["messaging_product"] != "whatsapp" {
		t.Errorf("profile body = %v", b)
	}
}

func TestSetPhotoRejectsBadImages(t *testing.T) {
	g, srv := newFakeMeta(t)
	s, _ := newTestService(t, srv.URL, nil)
	for name, data := range map[string][]byte{
		"too small": testPNG(t, 100, 100),
		"gif":       []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"not image": []byte("hello"),
		"too large": append(testJPEG(t, 300, 300), make([]byte, MaxPhotoBytes)...),
	} {
		if err := s.SetPhotoBytes(context.Background(), "t1", data); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if len(g.calls) != 0 {
		t.Fatalf("bad images reached Meta: %v", g.calls)
	}
}

func TestSetPhotoUploadFailurePassesThrough(t *testing.T) {
	g, srv := newFakeMeta(t)
	g.failPath = "/v26.0/upload:SESSION1"
	g.failBody = `{"error":{"message":"Invalid OAuth access token","code":190}}`
	s, _ := newTestService(t, srv.URL, nil)
	err := s.SetPhotoBytes(context.Background(), "t1", testJPEG(t, 300, 300))
	var ge *GraphError
	if !errors.As(err, &ge) || ge.Code != 190 {
		t.Fatalf("err = %v, want the upload's GraphError", err)
	}
	if g.called("POST /v26.0/P-t1/whatsapp_business_profile") {
		t.Fatal("profile updated without a handle")
	}
}

func TestSetPhotoFromURL(t *testing.T) {
	g, srv := newFakeMeta(t)
	img := testJPEG(t, 400, 400)
	logo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png") // mislabeled on purpose: the bytes decide
		_, _ = w.Write(img)
	}))
	t.Cleanup(logo.Close)
	s, _ := newTestService(t, srv.URL, nil)
	if err := s.SetPhotoFromURL(context.Background(), "t1", logo.URL+"/logo.jpg"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.queries["POST /v26.0/APP1/uploads"], "file_type=image%2Fjpeg") {
		t.Errorf("session query = %q, want the sniffed JPEG type", g.queries["POST /v26.0/APP1/uploads"])
	}
	if err := s.SetPhotoFromURL(context.Background(), "t1", "ftp://example.com/logo.png"); !errors.Is(err, ErrInvalid) {
		t.Errorf("ftp url: err = %v, want ErrInvalid", err)
	}
}

// The default image client refuses hosts on private networks, so a tenant admin cannot make the
// service fetch from inside the cluster.
func TestDefaultImageClientRefusesPrivateHosts(t *testing.T) {
	logo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }))
	t.Cleanup(logo.Close)
	s := New(nil, nil, nil, zap.NewNop())
	if _, err := s.fetchImage(context.Background(), logo.URL); err == nil {
		t.Fatal("fetched from a loopback host")
	}
}

func TestSyncFromBrandingFillsOnlyEmptyFields(t *testing.T) {
	g, srv := newFakeMeta(t)
	g.profile = `{"about":"Our own words","address":"","description":"","email":"","websites":[],"vertical":"","messaging_product":"whatsapp"}`
	img := testPNG(t, 300, 300)
	logo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(img) }))
	t.Cleanup(logo.Close)
	s, _ := newTestService(t, srv.URL, map[string]Branding{"t1": {
		Name: "Hadia", About: "Tagline from branding", Description: "Hadia sells food", Address: "Moi Ave, Nairobi",
		Email: "hello@hadia.example", Website: "hadia.example", LogoURL: logo.URL + "/logo.png",
	}})

	rep, err := s.SyncFromBranding(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	b := g.body("POST /v26.0/P-t1/whatsapp_business_profile")
	if _, ok := b["about"]; ok {
		t.Errorf("about was set already and must be kept, body = %v", b)
	}
	if b["email"] != "hello@hadia.example" || b["address"] != "Moi Ave, Nairobi" || b["description"] != "Hadia sells food" {
		t.Errorf("body = %v", b)
	}
	if ws, _ := b["websites"].([]any); len(ws) != 1 || ws[0] != "https://hadia.example" {
		t.Errorf("websites = %v", b["websites"])
	}
	if !g.called("POST /v26.0/APP1/uploads") {
		t.Error("logo not uploaded although the profile has no photo")
	}
	filled := strings.Join(rep.Filled, ",")
	for _, f := range []string{"email", "address", "description", "websites", "photo"} {
		if !strings.Contains(filled, f) {
			t.Errorf("filled %v misses %s", rep.Filled, f)
		}
	}
	if strings.Contains(filled, "about") {
		t.Errorf("filled %v claims about", rep.Filled)
	}
}

func TestSyncFromBrandingKeepsExistingPhotoAndSkipsWhenComplete(t *testing.T) {
	g, srv := newFakeMeta(t)
	g.profile = `{"about":"a","address":"b","description":"c","email":"d@e.co","websites":["https://x.co"],"vertical":"RETAIL","profile_picture_url":"https://pps.example/p.jpg","messaging_product":"whatsapp"}`
	s, _ := newTestService(t, srv.URL, map[string]Branding{"t1": {Name: "Hadia", About: "New", Email: "z@z.co", Website: "z.co", LogoURL: "https://logo.example/l.png"}})
	rep, err := s.SyncFromBranding(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if g.called("POST ") {
		t.Fatalf("nothing was empty, yet Meta was written: %v", g.calls)
	}
	if len(rep.Filled) != 0 {
		t.Errorf("filled = %v", rep.Filled)
	}
}

// A logo that cannot be used never stops the text fields from being filled.
func TestSyncFromBrandingLogoFailureIsAWarning(t *testing.T) {
	g, srv := newFakeMeta(t)
	logo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<svg/>")) }))
	t.Cleanup(logo.Close)
	s, _ := newTestService(t, srv.URL, map[string]Branding{"t1": {Name: "Hadia", Email: "hello@hadia.example", LogoURL: logo.URL}})
	rep, err := s.SyncFromBranding(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if g.body("POST /v26.0/P-t1/whatsapp_business_profile")["email"] != "hello@hadia.example" {
		t.Error("email not filled")
	}
	if len(rep.Warnings) == 0 {
		t.Error("an unusable logo must come back as a warning")
	}
}

func TestSyncFromBrandingUsesTheTenantsOwnData(t *testing.T) {
	g, srv := newFakeMeta(t)
	s, asked := newTestService(t, srv.URL, map[string]Branding{
		"t1": {Email: "one@t1.example"},
		"t2": {Email: "two@t2.example"},
	})
	if _, err := s.SyncFromBranding(context.Background(), "t2"); err != nil {
		t.Fatal(err)
	}
	if b := g.body("POST /v26.0/P-t2/whatsapp_business_profile"); b["email"] != "two@t2.example" {
		t.Errorf("t2 body = %v", b)
	}
	if g.called("POST /v26.0/P-t1") {
		t.Fatal("t2's sync wrote to t1's number")
	}
	for _, id := range *asked {
		if id != "t2" {
			t.Fatalf("credentials asked for %q", id)
		}
	}
}

func TestBrandingFromAboutIsCutToMetaLimit(t *testing.T) {
	g, srv := newFakeMeta(t)
	s, _ := newTestService(t, srv.URL, map[string]Branding{"t1": {About: strings.Repeat("é", 200)}})
	if _, err := s.SyncFromBranding(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	about, _ := g.body("POST /v26.0/P-t1/whatsapp_business_profile")["about"].(string)
	if n := len([]rune(about)); n == 0 || n > MaxAbout {
		t.Fatalf("about has %d characters, want 1..%d", n, MaxAbout)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
