package templatecat

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// Account is one WhatsApp Business Account we send from, with a token that can read it.
type Account struct {
	WABAID     string
	Token      string
	APIVersion string
}

// Fetcher lists a WABA's templates with Meta's category (templatesync.Syncer.FetchExisting).
type Fetcher func(ctx context.Context, a Account) (map[string]templatesync.ExistingTemplate, error)

// GraphFetcher reads templates from Meta's Graph API.
func GraphFetcher(ctx context.Context, a Account) (map[string]templatesync.ExistingTemplate, error) {
	sy := templatesync.NewSyncer(a.WABAID, a.Token)
	sy.APIVersion = a.APIVersion
	return sy.FetchExisting(ctx)
}

// SweepInterval is how often every WABA's categories are re-read. Meta's webhook normally tells
// us first; the sweep catches a move while the webhook field is not subscribed or a call is lost.
const SweepInterval = 6 * time.Hour

// Sweep re-reads every account's templates, records Meta's categories and alerts on each one that
// moved since the last sweep or webhook. Only one replica sweeps per interval (Claim). The first
// sweep only fills the store: with nothing to compare with, it alerts on nothing.
func (s *Store) Sweep(ctx context.Context, accounts []Account, fetch Fetcher, alert Alerter, log *zap.Logger) (moved int) {
	if s == nil || fetch == nil {
		return 0
	}
	if log == nil {
		log = zap.NewNop()
	}
	if !s.Claim(ctx, "sweep", SweepInterval-10*time.Minute) {
		return 0
	}
	seen := map[string]bool{}
	for _, a := range accounts {
		if a.WABAID == "" || a.Token == "" || seen[a.WABAID] {
			continue
		}
		seen[a.WABAID] = true
		existing, err := fetch(ctx, a)
		if err != nil {
			log.Warn("whatsapp template sweep: Meta refused the template list", zap.String("waba_id", a.WABAID), zap.Error(err))
			continue
		}
		for name, mt := range existing {
			prev, changed, err := s.Set(ctx, a.WABAID, name, mt.Category)
			if err != nil {
				log.Warn("whatsapp template sweep: could not record a category", zap.String("template", name), zap.Error(err))
				continue
			}
			if !changed {
				continue
			}
			moved++
			if alert != nil {
				alert.AlertWhatsAppTemplateChange(ctx, Change{Kind: "category", WABAID: a.WABAID, Name: name,
					PreviousCategory: prev, NewCategory: mt.Category, Source: "sweep"})
			}
		}
	}
	log.Info("whatsapp template sweep finished", zap.Int("wabas", len(seen)), zap.Int("moved", moved))
	return moved
}

// RunSweeps sweeps after a short delay (so a rollout settles), then every SweepInterval.
func (s *Store) RunSweeps(ctx context.Context, accounts func(context.Context) ([]Account, error), fetch Fetcher, alert Alerter, log *zap.Logger) {
	if s == nil {
		return
	}
	go func() {
		timer := time.NewTimer(2 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if accs, err := accounts(ctx); err == nil {
				s.Sweep(ctx, accs, fetch, alert, log)
			} else if log != nil {
				log.Warn("whatsapp template sweep: could not list accounts", zap.Error(err))
			}
			timer.Reset(SweepInterval)
		}
	}()
}
