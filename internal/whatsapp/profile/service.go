// Package profile manages the WhatsApp business profile of a tenant's own Cloud API number: the
// about text, description, address, email, websites, category and profile photo customers see, plus
// the display name review state and quality rating Meta reports for the number.
//
// Meta references (checked 2026-11-12):
//   - Business profile: https://developers.facebook.com/docs/whatsapp/cloud-api/reference/business-profiles
//   - Resumable Upload API: https://developers.facebook.com/docs/graph-api/guides/upload
//   - Phone numbers and display name status: https://developers.facebook.com/docs/whatsapp/cloud-api/phone-numbers
//   - Display names: https://developers.facebook.com/documentation/business-messaging/whatsapp/display-names
//
// docs/integrations.md has the rules on when customers see the display name.
package profile

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registers the JPEG decoder for image.DecodeConfig
	_ "image/png"  // registers the PNG decoder for image.DecodeConfig
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/providers/whatsapp"
)

// Meta's limits on the business profile fields (Business Profiles reference).
const (
	MaxAbout       = 139
	MaxAddress     = 256
	MaxDescription = 512
	MaxEmail       = 128
	MaxWebsites    = 2
	MaxWebsite     = 256
)

// Profile photo limits. The current Cloud API reference names none beyond the upload API's file
// types (JPEG, PNG among them); these are Meta's published profile photo rules from the
// On-Premises API reference, which the Cloud API still applies in practice: at most 5 MB, square
// crop to at most 640 px, and images under 192 px on a side may break the resize.
const (
	MaxPhotoBytes = 5 << 20
	MinPhotoEdge  = 192
)

// Verticals are the business categories Meta accepts ("" clears it).
var Verticals = []string{
	"ALCOHOL", "APPAREL", "AUTO", "BEAUTY", "EDU", "ENTERTAIN", "EVENT_PLAN", "FINANCE", "GOVT", "GROCERY",
	"HEALTH", "HOTEL", "NONPROFIT", "ONLINE_GAMBLING", "OTC_DRUGS", "OTHER", "PHYSICAL_GAMBLING",
	"PROF_SERVICES", "RESTAURANT", "RETAIL", "TRAVEL",
}

// ErrCredentials wraps a failure to resolve the tenant's own number and token: the caller has to
// connect a number first (a client error, not Meta's).
var ErrCredentials = errors.New("tenant WhatsApp number credentials")

// ErrInvalid marks input Meta would refuse (field too long, bad email, unusable image).
var ErrInvalid = errors.New("invalid WhatsApp business profile")

// Credentials are a tenant's own Cloud API number, its business token and Graph version setting.
type Credentials struct {
	PhoneNumberID string
	AccessToken   string
	APIVersion    string
}

// CredentialsFunc resolves a tenant's own number (Manager.LoadTenantWhatsAppProfileCredentials).
type CredentialsFunc func(ctx context.Context, tenantID string) (Credentials, error)

// AppIDFunc resolves the WhatsApp app ID the upload sessions are opened on.
type AppIDFunc func(ctx context.Context) (string, error)

// Branding is the part of a tenant's branding (owned by auth-api) the profile can be filled from.
type Branding struct {
	Name        string
	About       string // the tenant's tagline
	Description string
	Address     string
	Email       string
	Website     string
	LogoURL     string
}

// BrandingFunc resolves a tenant's branding.
type BrandingFunc func(ctx context.Context, tenantID string) (Branding, error)

// Profile is what the settings page shows: the number's name and quality, and its profile.
type Profile struct {
	PhoneNumberID      string `json:"phone_number_id"`
	DisplayPhoneNumber string `json:"display_phone_number,omitempty"`
	// DisplayName is Meta's verified_name, NameStatus its review state (APPROVED, PENDING_REVIEW,
	// DECLINED, AVAILABLE_WITHOUT_REVIEW, EXPIRED, NONE).
	DisplayName string `json:"display_name,omitempty"`
	NameStatus  string `json:"name_status,omitempty"`
	// NewDisplayName and NewNameStatus describe a requested name change still being reviewed.
	NewDisplayName string `json:"new_display_name,omitempty"`
	NewNameStatus  string `json:"new_name_status,omitempty"`
	// QualityRating is GREEN, YELLOW, RED, or NA/UNKNOWN for a number with too little traffic.
	QualityRating string   `json:"quality_rating,omitempty"`
	About         string   `json:"about"`
	Address       string   `json:"address"`
	Description   string   `json:"description"`
	Email         string   `json:"email"`
	Websites      []string `json:"websites"`
	Vertical      string   `json:"vertical"`
	PhotoURL      string   `json:"photo_url,omitempty"`
}

// Update holds the profile fields to change; nil leaves a field as it is.
type Update struct {
	About       *string   `json:"about,omitempty"`
	Address     *string   `json:"address,omitempty"`
	Description *string   `json:"description,omitempty"`
	Email       *string   `json:"email,omitempty"`
	Websites    *[]string `json:"websites,omitempty"`
	Vertical    *string   `json:"vertical,omitempty"`
}

// SyncReport is the outcome of filling a profile from the tenant's branding.
type SyncReport struct {
	// Filled lists the fields set from the branding ("about", "email", "photo" ...).
	Filled []string `json:"filled"`
	// Warnings explain what could not be used (an SVG logo, a too long tagline ...).
	Warnings []string `json:"warnings,omitempty"`
}

// GraphError is Meta's own error answer, passed through so the tenant sees Meta's reason.
type GraphError struct {
	Status      int    `json:"status"`
	Code        int    `json:"code"`
	Subcode     int    `json:"error_subcode,omitempty"`
	Message     string `json:"message"`
	UserMessage string `json:"user_message,omitempty"`
	TraceID     string `json:"fbtrace_id,omitempty"`
}

func (e *GraphError) Error() string {
	msg := e.Message
	if e.UserMessage != "" {
		msg = e.UserMessage + " (" + e.Message + ")"
	}
	return fmt.Sprintf("Meta answered %d: %s", e.Status, msg)
}

// graphTimeout bounds each call to Meta; autoSyncTimeout a whole background sync after signup.
const (
	graphTimeout    = 60 * time.Second
	imageTimeout    = 20 * time.Second
	autoSyncTimeout = 3 * time.Minute
)

// Service reads and writes the business profile of a tenant's own number.
type Service struct {
	creds    CredentialsFunc
	appID    AppIDFunc
	branding BrandingFunc
	log      *zap.Logger
	// HTTP calls Meta. ImageClient downloads photos from URLs; the default one refuses hosts on
	// loopback and private networks, since the URL comes from a tenant admin.
	HTTP        *http.Client
	ImageClient *http.Client
	// GraphURL overrides Meta's Graph API base (tests only).
	GraphURL string
}

// New builds the service.
func New(creds CredentialsFunc, appID AppIDFunc, branding BrandingFunc, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{
		creds: creds, appID: appID, branding: branding,
		log:         log.Named("whatsapp-profile"),
		HTTP:        &http.Client{Timeout: graphTimeout},
		ImageClient: publicOnlyClient(),
		GraphURL:    "https://graph.facebook.com",
	}
}

// target is the resolved number for one call.
type target struct {
	base    string // graph base with version, no trailing slash
	phoneID string
	token   string
}

func (s *Service) target(ctx context.Context, tenantID string) (target, error) {
	if strings.TrimSpace(tenantID) == "" {
		return target{}, fmt.Errorf("%w: tenant id is required", ErrCredentials)
	}
	if s.creds == nil {
		return target{}, fmt.Errorf("%w: not configured", ErrCredentials)
	}
	c, err := s.creds(ctx, tenantID)
	if err != nil {
		return target{}, fmt.Errorf("%w: %w", ErrCredentials, err)
	}
	if c.PhoneNumberID == "" || c.AccessToken == "" {
		return target{}, fmt.Errorf("%w: no connected number or token", ErrCredentials)
	}
	return target{
		base:    strings.TrimRight(s.GraphURL, "/") + "/" + whatsapp.GraphVersion(c.APIVersion),
		phoneID: c.PhoneNumberID,
		token:   c.AccessToken,
	}, nil
}

// Get returns the number's display name state, quality rating and business profile.
func (s *Service) Get(ctx context.Context, tenantID string) (Profile, error) {
	t, err := s.target(ctx, tenantID)
	if err != nil {
		return Profile{}, err
	}
	var phone struct {
		VerifiedName       string `json:"verified_name"`
		NameStatus         string `json:"name_status"`
		NewDisplayName     string `json:"new_display_name"`
		NewNameStatus      string `json:"new_name_status"`
		DisplayPhoneNumber string `json:"display_phone_number"`
		QualityRating      string `json:"quality_rating"`
	}
	q := url.Values{"fields": {"verified_name,name_status,new_display_name,new_name_status,display_phone_number,quality_rating"}}
	if err := s.do(ctx, http.MethodGet, t.base+"/"+t.phoneID+"?"+q.Encode(), "Bearer "+t.token, nil, nil, &phone); err != nil {
		return Profile{}, err
	}
	var prof struct {
		Data []struct {
			About             string   `json:"about"`
			Address           string   `json:"address"`
			Description       string   `json:"description"`
			Email             string   `json:"email"`
			Websites          []string `json:"websites"`
			Vertical          string   `json:"vertical"`
			ProfilePictureURL string   `json:"profile_picture_url"`
		} `json:"data"`
	}
	q = url.Values{"fields": {"about,address,description,email,profile_picture_url,websites,vertical"}}
	if err := s.do(ctx, http.MethodGet, t.base+"/"+t.phoneID+"/whatsapp_business_profile?"+q.Encode(), "Bearer "+t.token, nil, nil, &prof); err != nil {
		return Profile{}, err
	}
	p := Profile{
		PhoneNumberID: t.phoneID, DisplayPhoneNumber: phone.DisplayPhoneNumber,
		DisplayName: phone.VerifiedName, NameStatus: phone.NameStatus,
		NewDisplayName: phone.NewDisplayName, NewNameStatus: phone.NewNameStatus,
		QualityRating: phone.QualityRating, Websites: []string{},
	}
	if len(prof.Data) > 0 {
		d := prof.Data[0]
		p.About, p.Address, p.Description, p.Email = d.About, d.Address, d.Description, d.Email
		p.Vertical, p.PhotoURL = d.Vertical, d.ProfilePictureURL
		if d.Websites != nil {
			p.Websites = d.Websites
		}
	}
	return p, nil
}

// Update validates the given fields against Meta's limits and writes them to the profile.
func (s *Service) Update(ctx context.Context, tenantID string, u Update) error {
	body, err := u.body()
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	t, err := s.target(ctx, tenantID)
	if err != nil {
		return err
	}
	return s.postProfile(ctx, t, body)
}

func (s *Service) postProfile(ctx context.Context, t target, body map[string]any) error {
	body["messaging_product"] = "whatsapp"
	raw, _ := json.Marshal(body)
	return s.do(ctx, http.MethodPost, t.base+"/"+t.phoneID+"/whatsapp_business_profile", "Bearer "+t.token,
		map[string]string{"Content-Type": "application/json"}, raw, nil)
}

// body turns the update into Meta's request fields, refusing what Meta would refuse.
func (u Update) body() (map[string]any, error) {
	out := map[string]any{}
	if u.About != nil {
		v := strings.TrimSpace(*u.About)
		if v == "" {
			return nil, fmt.Errorf("%w: the about text cannot be empty", ErrInvalid)
		}
		if n := len([]rune(v)); n > MaxAbout {
			return nil, fmt.Errorf("%w: the about text is %d characters, at most %d", ErrInvalid, n, MaxAbout)
		}
		out["about"] = v
	}
	if err := limited(out, "address", u.Address, MaxAddress); err != nil {
		return nil, err
	}
	if err := limited(out, "description", u.Description, MaxDescription); err != nil {
		return nil, err
	}
	if u.Email != nil {
		v := strings.TrimSpace(*u.Email)
		if v != "" {
			if len([]rune(v)) > MaxEmail {
				return nil, fmt.Errorf("%w: the email is longer than %d characters", ErrInvalid, MaxEmail)
			}
			if a, err := mail.ParseAddress(v); err != nil || a.Address != v {
				return nil, fmt.Errorf("%w: %q is not an email address", ErrInvalid, v)
			}
		}
		out["email"] = v
	}
	if u.Websites != nil {
		sites, err := normalizeWebsites(*u.Websites)
		if err != nil {
			return nil, err
		}
		out["websites"] = sites
	}
	if u.Vertical != nil {
		v := strings.ToUpper(strings.TrimSpace(*u.Vertical))
		if v != "" && !validVertical(v) {
			return nil, fmt.Errorf("%w: %q is not a business category Meta knows", ErrInvalid, v)
		}
		out["vertical"] = v
	}
	return out, nil
}

func limited(out map[string]any, key string, v *string, max int) error {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(*v)
	if n := len([]rune(s)); n > max {
		return fmt.Errorf("%w: the %s is %d characters, at most %d", ErrInvalid, key, n, max)
	}
	out[key] = s
	return nil
}

func validVertical(v string) bool {
	for _, x := range Verticals {
		if x == v {
			return true
		}
	}
	return false
}

// normalizeWebsites drops blanks, adds https:// where no scheme is given and enforces Meta's
// two-site, 256-character limits.
func normalizeWebsites(in []string) ([]string, error) {
	out := []string{}
	for _, w := range in {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		w = normalizeWebsite(w)
		if len(w) > MaxWebsite {
			return nil, fmt.Errorf("%w: a website is longer than %d characters", ErrInvalid, MaxWebsite)
		}
		if u, err := url.Parse(w); err != nil || u.Host == "" {
			return nil, fmt.Errorf("%w: %q is not a website address", ErrInvalid, w)
		}
		out = append(out, w)
	}
	if len(out) > MaxWebsites {
		return nil, fmt.Errorf("%w: at most %d websites", ErrInvalid, MaxWebsites)
	}
	return out, nil
}

func normalizeWebsite(w string) string {
	w = strings.TrimSpace(w)
	lower := strings.ToLower(w)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		w = "https://" + w
	}
	return w
}

// ValidateImage checks a profile photo: JPEG or PNG by its bytes (never a declared type), at
// most MaxPhotoBytes, at least MinPhotoEdge pixels on each side. It returns the MIME type.
func ValidateImage(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("%w: the photo is empty", ErrInvalid)
	}
	if len(data) > MaxPhotoBytes {
		return "", fmt.Errorf("%w: the photo is larger than 5 MB", ErrInvalid)
	}
	mime := http.DetectContentType(data)
	if mime != "image/jpeg" && mime != "image/png" {
		return "", fmt.Errorf("%w: the photo must be a JPEG or PNG image", ErrInvalid)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%w: the photo could not be read as an image", ErrInvalid)
	}
	if cfg.Width < MinPhotoEdge || cfg.Height < MinPhotoEdge {
		return "", fmt.Errorf("%w: the photo is %dx%d pixels, it needs at least %dx%d (640x640 is best)",
			ErrInvalid, cfg.Width, cfg.Height, MinPhotoEdge, MinPhotoEdge)
	}
	return mime, nil
}

// SetPhotoBytes validates the image, uploads it with Meta's Resumable Upload API and sets the
// handle as the profile photo.
func (s *Service) SetPhotoBytes(ctx context.Context, tenantID string, data []byte) error {
	mime, err := ValidateImage(data)
	if err != nil {
		return err
	}
	t, err := s.target(ctx, tenantID)
	if err != nil {
		return err
	}
	handle, err := s.upload(ctx, t, data, mime)
	if err != nil {
		return err
	}
	return s.postProfile(ctx, t, map[string]any{"profile_picture_handle": handle})
}

// SetPhotoFromURL downloads an image (http, https or an inline data: URI) and sets it as the photo.
func (s *Service) SetPhotoFromURL(ctx context.Context, tenantID, imageURL string) error {
	data, err := s.fetchImage(ctx, imageURL)
	if err != nil {
		return err
	}
	return s.SetPhotoBytes(ctx, tenantID, data)
}

// upload runs the two steps of the Resumable Upload API: open a session on the app, then send the
// bytes with file_offset 0 and an OAuth (not Bearer) header. It returns the file handle.
func (s *Service) upload(ctx context.Context, t target, data []byte, mime string) (string, error) {
	if s.appID == nil {
		return "", errors.New("the WhatsApp app ID is not configured")
	}
	appID, err := s.appID(ctx)
	if err != nil || strings.TrimSpace(appID) == "" {
		return "", errors.New("the WhatsApp app ID is not set: add it under Platform > Providers > WhatsApp (Meta Cloud)")
	}
	ext := ".jpg"
	if mime == "image/png" {
		ext = ".png"
	}
	q := url.Values{
		"file_name":   {"profile" + ext},
		"file_length": {strconv.Itoa(len(data))},
		"file_type":   {mime},
	}
	var session struct {
		ID string `json:"id"`
	}
	if err := s.do(ctx, http.MethodPost, t.base+"/"+appID+"/uploads?"+q.Encode(), "Bearer "+t.token, nil, nil, &session); err != nil {
		return "", err
	}
	if !strings.HasPrefix(session.ID, "upload:") {
		return "", errors.New("Meta did not open an upload session")
	}
	// The session id is used as given: it can carry its own query (?sig=...).
	var up struct {
		H string `json:"h"`
	}
	if err := s.do(ctx, http.MethodPost, t.base+"/"+session.ID, "OAuth "+t.token,
		map[string]string{"file_offset": "0", "Content-Type": mime}, data, &up); err != nil {
		return "", err
	}
	if up.H == "" {
		return "", errors.New("Meta returned no handle for the uploaded photo")
	}
	return up.H, nil
}

// do sends one Graph call and decodes the answer into out. A non-2xx answer becomes a GraphError
// with Meta's own message; errors never carry the URL or the token.
func (s *Service) do(ctx context.Context, method, rawURL, auth string, headers map[string]string, body []byte, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return errors.New("could not build the request to Meta")
	}
	req.Header.Set("Authorization", auth)
	for k, v := range headers {
		// Set as written: Meta documents the upload header as lowercase "file_offset".
		req.Header[k] = []string{v}
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Meta did not answer in time: %w", ctx.Err())
		}
		return errors.New("could not reach Meta")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		ge := &GraphError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		var env struct {
			Error struct {
				Message   string `json:"message"`
				Code      int    `json:"code"`
				Subcode   int    `json:"error_subcode"`
				UserMsg   string `json:"error_user_msg"`
				FBTraceID string `json:"fbtrace_id"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Error.Message != "" {
			ge.Message, ge.Code, ge.Subcode = env.Error.Message, env.Error.Code, env.Error.Subcode
			ge.UserMessage, ge.TraceID = env.Error.UserMsg, env.Error.FBTraceID
		}
		if len(ge.Message) > 500 {
			ge.Message = ge.Message[:500]
		}
		return ge
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("Meta's answer could not be read")
	}
	return nil
}

// fetchImage downloads a photo, reading at most one byte past the limit so an oversized image is
// refused rather than cut. A data: URI (logos saved inline) is decoded in place.
func (s *Service) fetchImage(ctx context.Context, imageURL string) ([]byte, error) {
	imageURL = strings.TrimSpace(imageURL)
	if strings.HasPrefix(imageURL, "data:") {
		return decodeDataURI(imageURL)
	}
	u, err := url.Parse(imageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: the photo address must start with http:// or https://", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, imageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: bad photo address", ErrInvalid)
	}
	resp, err := s.ImageClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: the photo could not be downloaded", ErrInvalid)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: the photo address answered %d", ErrInvalid, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxPhotoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: the photo could not be downloaded", ErrInvalid)
	}
	return data, nil
}

func decodeDataURI(s string) ([]byte, error) {
	comma := strings.IndexByte(s, ',')
	if comma < 0 || !strings.Contains(s[:comma], ";base64") {
		return nil, fmt.Errorf("%w: the inline photo is not base64", ErrInvalid)
	}
	enc := s[comma+1:]
	if base64.StdEncoding.DecodedLen(len(enc)) > MaxPhotoBytes+3 {
		return nil, fmt.Errorf("%w: the photo is larger than 5 MB", ErrInvalid)
	}
	data, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("%w: the inline photo could not be decoded", ErrInvalid)
	}
	return data, nil
}

// publicOnlyClient downloads only from public addresses: every connection, redirects included,
// is refused when it would reach loopback, private, link-local or carrier-grade NAT space.
func publicOnlyClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !publicIP(ip) {
				return errors.New("photo host is not on the public internet")
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: imageTimeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func publicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip))
}

// SyncFromBranding fills the profile fields that are still empty, and the photo when none is set,
// from the tenant's branding. Fields the tenant already set are never overwritten. A logo that
// cannot be used is reported as a warning and does not stop the text fields.
func (s *Service) SyncFromBranding(ctx context.Context, tenantID string) (SyncReport, error) {
	rep := SyncReport{Filled: []string{}}
	if s.branding == nil {
		return rep, errors.New("tenant branding is not configured")
	}
	cur, err := s.Get(ctx, tenantID)
	if err != nil {
		return rep, err
	}
	b, err := s.branding(ctx, tenantID)
	if err != nil {
		return rep, fmt.Errorf("could not read the business's branding: %w", err)
	}

	var u Update
	var filled []string
	if cur.About == "" {
		if v := cutRunes(strings.TrimSpace(b.About), MaxAbout); v != "" {
			u.About, filled = &v, append(filled, "about")
		}
	}
	if cur.Description == "" {
		if v := cutRunes(strings.TrimSpace(b.Description), MaxDescription); v != "" {
			u.Description, filled = &v, append(filled, "description")
		}
	}
	if cur.Address == "" {
		if v := cutRunes(strings.TrimSpace(b.Address), MaxAddress); v != "" {
			u.Address, filled = &v, append(filled, "address")
		}
	}
	if cur.Email == "" {
		if v := strings.TrimSpace(b.Email); v != "" {
			if a, perr := mail.ParseAddress(v); perr == nil && a.Address == v && len(v) <= MaxEmail {
				u.Email, filled = &v, append(filled, "email")
			} else {
				rep.Warnings = append(rep.Warnings, "the business email is not a valid address, so it was left out")
			}
		}
	}
	if len(cur.Websites) == 0 {
		if v := strings.TrimSpace(b.Website); v != "" {
			if sites, werr := normalizeWebsites([]string{v}); werr == nil && len(sites) == 1 {
				u.Websites, filled = &sites, append(filled, "websites")
			} else {
				rep.Warnings = append(rep.Warnings, "the business website is not a valid address, so it was left out")
			}
		}
	}
	// The photo goes first: a logo Meta refuses is only a warning and never holds back the text.
	if cur.PhotoURL == "" && strings.TrimSpace(b.LogoURL) != "" {
		if err := s.SetPhotoFromURL(ctx, tenantID, b.LogoURL); err != nil {
			rep.Warnings = append(rep.Warnings, "the logo could not be used as the profile photo: "+err.Error())
		} else {
			rep.Filled = append(rep.Filled, "photo")
		}
	}

	if len(filled) > 0 {
		if err := s.Update(ctx, tenantID, u); err != nil {
			return rep, err
		}
		rep.Filled = append(rep.Filled, filled...)
	}
	return rep, nil
}

// AutoSync fills the profile from branding in the background after Embedded Signup. It never
// fails the signup: the outcome is only logged.
func (s *Service) AutoSync(ctx context.Context, tenantID string) {
	ctx, cancel := context.WithTimeout(ctx, autoSyncTimeout)
	defer cancel()
	rep, err := s.SyncFromBranding(ctx, tenantID)
	if err != nil {
		s.log.Warn("whatsapp profile auto-sync after signup failed", zap.String("tenant_id", tenantID), zap.Error(err))
		return
	}
	s.log.Info("whatsapp profile filled from branding after signup",
		zap.String("tenant_id", tenantID), zap.Strings("filled", rep.Filled), zap.Strings("warnings", rep.Warnings))
}

func cutRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max]))
}
