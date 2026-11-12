package tenanttemplates

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// Credentials resolves a tenant's own WABA ID and access token (Manager.LoadTenantWhatsAppTemplateCredentials).
type Credentials func(ctx context.Context, tenantID string) (wabaID, accessToken string, err error)

// ErrCredentials wraps a failure to resolve the tenant's WABA credentials, so callers can answer
// with a client error (connect a number first) rather than a gateway error.
var ErrCredentials = errors.New("tenant WhatsApp Business Account credentials")

// BatchSize is how many templates one sync call submits, the platform sync's own upper limit
// (handlers.maxSyncBatch); the rest come back queued with Remaining counting them.
const BatchSize = 10

// autoSyncTimeout bounds a whole background sync after Embedded Signup.
const autoSyncTimeout = 10 * time.Minute

// Service syncs and reports the selected templates on a tenant's own WABA.
type Service struct {
	creds     Credentials
	usage     []UsageSource
	selection Selection
	log       *zap.Logger
	manifest  func() ([]templatesync.TemplateDef, error)
	// GraphURL overrides Meta's Graph API base (tests only).
	GraphURL string
}

// New builds the service. usage sources are unioned; a source that fails is logged and skipped,
// so the shared templates are still selected.
func New(creds Credentials, selection Selection, log *zap.Logger, usage ...UsageSource) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{
		creds:     creds,
		usage:     usage,
		selection: selection,
		log:       log.Named("tenant-whatsapp-templates"),
		manifest:  templatesync.LoadManifest,
	}
}

// SyncReport is one sync call's outcome.
type SyncReport struct {
	WABAID    string                `json:"waba_id"`
	Services  []string              `json:"services"`
	Results   []templatesync.Result `json:"results"`
	Summary   map[string]int        `json:"summary"`
	Remaining int                   `json:"remaining"`
}

// TemplateStatus is one selected template's state on the tenant's WABA.
type TemplateStatus struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	// Status is Meta's review status (APPROVED, PENDING, REJECTED, PAUSED, DISABLED ...), or
	// MISSING when the template has not been submitted to this WABA.
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// StatusMissing marks a selected template not yet on the tenant's WABA.
const StatusMissing = "MISSING"

// StatusReport lists every selected template with its Meta status.
type StatusReport struct {
	WABAID    string           `json:"waba_id"`
	Services  []string         `json:"services"`
	Templates []TemplateStatus `json:"templates"`
	Summary   map[string]int   `json:"summary"`
}

func (s *Service) syncer(wabaID, token string) *templatesync.Syncer {
	sy := templatesync.NewSyncer(wabaID, token)
	sy.GraphURL = s.GraphURL
	return sy
}

// selected resolves the tenant's credentials and the templates it uses.
func (s *Service) selected(ctx context.Context, tenantID string) (string, string, []templatesync.TemplateDef, []string, error) {
	wabaID, token, err := s.creds(ctx, tenantID)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("%w: %w", ErrCredentials, err)
	}
	defs, err := s.manifest()
	if err != nil {
		return "", "", nil, nil, err
	}
	var services []string
	for _, src := range s.usage {
		got, uerr := src(ctx, tenantID)
		if uerr != nil {
			s.log.Warn("could not read a tenant usage source, continuing without it", zap.String("tenant_id", tenantID), zap.Error(uerr))
			continue
		}
		services = append(services, got...)
	}
	picked, matched := s.selection.Select(defs, services)
	if matched == nil {
		matched = []string{}
	}
	return wabaID, token, picked, matched, nil
}

// SyncTenantTemplates submits the tenant's selected templates that are not yet on its WABA, at
// most BatchSize per call (Remaining counts the rest; call again until it is 0). dryRun reports
// what would be submitted without any write to Meta.
func (s *Service) SyncTenantTemplates(ctx context.Context, tenantID string, dryRun bool) (*SyncReport, error) {
	wabaID, token, defs, services, err := s.selected(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	results, remaining, err := s.syncer(wabaID, token).RunBatch(ctx, defs, dryRun, BatchSize)
	if err != nil {
		return nil, err
	}
	summary := map[string]int{}
	for _, res := range results {
		key := string(res.Outcome)
		if res.DryRun && res.Outcome == templatesync.OutcomeCreated {
			key = "would_create"
		}
		summary[key]++
		if res.Outcome == templatesync.OutcomeFailed {
			s.log.Warn("whatsapp template refused by Meta for a tenant WABA",
				zap.String("tenant_id", tenantID), zap.String("template", res.Name), zap.String("detail", res.Detail))
		}
	}
	if results == nil {
		results = []templatesync.Result{}
	}
	return &SyncReport{WABAID: wabaID, Services: services, Results: results, Summary: summary, Remaining: remaining}, nil
}

// TenantTemplateStatus lists each selected template with its status on the tenant's WABA.
func (s *Service) TenantTemplateStatus(ctx context.Context, tenantID string) (*StatusReport, error) {
	wabaID, token, defs, services, err := s.selected(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	existing, err := s.syncer(wabaID, token).FetchExisting(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch templates from Meta: %w", err)
	}
	out := make([]TemplateStatus, 0, len(defs))
	summary := map[string]int{}
	for _, def := range defs {
		st := TemplateStatus{Name: def.Name, Category: def.Category, Status: StatusMissing}
		if mt, ok := existing[def.Name]; ok {
			st.Status = mt.Status
			st.Reason = mt.RejectedReason
		}
		summary[st.Status]++
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return &StatusReport{WABAID: wabaID, Services: services, Templates: out, Summary: summary}, nil
}

// AutoSync submits every selected template, batch after batch, and logs the outcome. It is
// started in the background after Embedded Signup: it never returns an error, and it stops when
// nothing is left, when a batch makes no progress, or at autoSyncTimeout.
func (s *Service) AutoSync(ctx context.Context, tenantID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoSyncTimeout)
	defer cancel()

	created := 0
	failed := map[string]bool{} // a refused template is retried by the next batch; count it once
	lastRemaining := -1
	for {
		rep, err := s.SyncTenantTemplates(ctx, tenantID, false)
		if err != nil {
			s.log.Error("whatsapp template auto-sync after signup failed",
				zap.String("tenant_id", tenantID), zap.Int("created", created), zap.Error(err))
			return
		}
		for _, r := range rep.Results {
			switch r.Outcome {
			case templatesync.OutcomeCreated:
				created++
				delete(failed, r.Name)
			case templatesync.OutcomeFailed:
				failed[r.Name] = true
			}
		}
		if rep.Remaining == 0 || rep.Remaining == lastRemaining {
			s.log.Info("whatsapp template auto-sync after signup finished",
				zap.String("tenant_id", tenantID), zap.String("waba_id", rep.WABAID), zap.Strings("services", rep.Services),
				zap.Int("created", created), zap.Int("failed", len(failed)), zap.Int("remaining", rep.Remaining))
			return
		}
		lastRemaining = rep.Remaining
	}
}
