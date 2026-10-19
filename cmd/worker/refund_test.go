package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/modules/credits"
)

func refundFixture(t *testing.T) (*credits.Client, *int32) {
	t.Helper()
	var refunds int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refunds, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	cfg := serviceclient.DefaultConfig(srv.URL, "subscriptions-api", zap.NewNop())
	cfg.MaxElapsedTime = 300 * time.Millisecond
	return credits.New(serviceclient.New(cfg), "k"), &refunds
}

func TestRefundOnlyWhenChargedAndTheSendReallyFailed(t *testing.T) {
	cc, refunds := refundFixture(t)
	ctx := context.Background()
	tid := uuid.New()
	charged := credits.Decision{Allowed: true}

	failed := errors.New("provider rejected")
	refundOnFailure(ctx, cc, zap.NewNop(), &failed, charged, tid, credits.ServiceSMS, "n1:a1")
	if atomic.LoadInt32(refunds) != 1 {
		t.Fatalf("a failed charged send must refund once, got %d", *refunds)
	}

	var ok error
	refundOnFailure(ctx, cc, zap.NewNop(), &ok, charged, tid, credits.ServiceSMS, "n2:a1")
	skip := skipped("no recipient")
	refundOnFailure(ctx, cc, zap.NewNop(), &skip, charged, tid, credits.ServiceSMS, "n3:a1")
	exempt := credits.Decision{Allowed: true, Exempt: true}
	refundOnFailure(ctx, cc, zap.NewNop(), &failed, exempt, tid, credits.ServiceSMS, "n4:a1")
	refused := credits.Decision{Allowed: false}
	refundOnFailure(ctx, cc, zap.NewNop(), &failed, refused, tid, credits.ServiceSMS, "n5:a1")
	if atomic.LoadInt32(refunds) != 1 {
		t.Fatalf("success, skip, exempt and refused sends must not refund, got %d calls", *refunds)
	}
}
