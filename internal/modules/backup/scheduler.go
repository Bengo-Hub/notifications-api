package backup

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// schedulerAdvisoryLockKey is a fixed, service-unique key for the Postgres session advisory
// lock that guards the run so only ONE replica executes it. ("notifications-backup")
const schedulerAdvisoryLockKey int64 = 0x4E54_4642 // 'N','T','F','B'

// SchedulerConfig configures the hourly auto-backup + retention churn.
type SchedulerConfig struct {
	Enabled       bool // BACKUP_SCHEDULE_ENABLED (default true)
	Hour          int  // BACKUP_SCHEDULE_HOUR (default 2) — service-local time (legacy/global; per-tenant hour wins)
	RetentionDays int  // BACKUP_RETENTION_DAYS (default 4) — safety churn window
}

// Scheduler wakes at every top of the hour and backs up ONLY tenants that have opted in
// (auto_enabled=true) AND scheduled that hour. Auto-backup is OPT-IN: tenants that have not
// activated it are never touched. A Postgres advisory lock ensures only one replica works.
type Scheduler struct {
	svc *Service
	cfg SchedulerConfig
	log *zap.Logger
}

// NewScheduler builds the scheduler. Defaults are applied for zero-value config.
func NewScheduler(svc *Service, cfg SchedulerConfig, log *zap.Logger) *Scheduler {
	if cfg.Hour < 0 || cfg.Hour > 23 {
		cfg.Hour = 2
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = DefaultRetentionDays
	}
	return &Scheduler{svc: svc, cfg: cfg, log: log.Named("backup.Scheduler")}
}

// Start launches the scheduler goroutine: a churn-only pass on startup (backupHour=-1), then
// a guarded run at every top of the hour for that hour's activated tenants. Stops on ctx done.
func (sc *Scheduler) Start(ctx context.Context) {
	if !sc.cfg.Enabled {
		sc.log.Info("backup scheduler disabled (BACKUP_SCHEDULE_ENABLED=false)")
		return
	}
	sc.log.Info("backup scheduler started (opt-in per-tenant auto-backup)",
		zap.Int("retention_days", sc.cfg.RetentionDays))

	go func() {
		// Startup: churn only (backupHour=-1 skips backups).
		sc.runGuarded(ctx, -1)
		for {
			next := nextTopOfHour(time.Now())
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				sc.runGuarded(ctx, time.Now().Hour())
			}
		}
	}()
}

// runGuarded acquires the advisory lock and, if won, backs up the tenants that activated
// auto-backup for backupHour (when backupHour>=0) then runs a safety churn. Only one replica
// wins the lock per tick.
func (sc *Scheduler) runGuarded(ctx context.Context, backupHour int) {
	conn, err := sc.svc.db.Conn(ctx)
	if err != nil {
		sc.log.Warn("scheduler: acquire conn failed", zap.Error(err))
		return
	}
	defer func() { _ = conn.Close() }()

	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, schedulerAdvisoryLockKey).Scan(&got); err != nil {
		sc.log.Warn("scheduler: advisory lock failed", zap.Error(err))
		return
	}
	if !got {
		return
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schedulerAdvisoryLockKey)
	}()

	if backupHour >= 0 {
		sc.backupActivatedTenants(ctx, backupHour)
	}
	if _, err := sc.svc.Churn(ctx, sc.cfg.RetentionDays); err != nil {
		sc.log.Warn("scheduler: churn failed", zap.Error(err))
	}
}

// backupActivatedTenants backs up ONLY tenants that opted in (auto_enabled=true) for the
// given hour, then churns each tenant to its own configured retention.
func (sc *Scheduler) backupActivatedTenants(ctx context.Context, hour int) {
	tenants, err := sc.svc.ListActivatedTenants(ctx, hour)
	if err != nil {
		sc.log.Warn("scheduler: list activated tenants failed", zap.Error(err))
		return
	}
	if len(tenants) == 0 {
		return
	}
	ok := 0
	for _, t := range tenants {
		if _, err := sc.svc.Generate(ctx, t.TenantID); err != nil {
			sc.log.Warn("scheduler: tenant backup failed", zap.String("tenant", t.TenantID.String()), zap.Error(err))
			continue
		}
		if _, err := sc.svc.ChurnTenant(ctx, t.TenantID, t.RetentionDays); err != nil {
			sc.log.Warn("scheduler: tenant churn failed", zap.String("tenant", t.TenantID.String()), zap.Error(err))
		}
		ok++
	}
	sc.log.Info("scheduled auto-backup complete", zap.Int("hour", hour), zap.Int("activated", len(tenants)), zap.Int("succeeded", ok))
}

// nextTopOfHour returns the next top of the hour strictly after now.
func nextTopOfHour(now time.Time) time.Time {
	return now.Truncate(time.Hour).Add(time.Hour)
}
