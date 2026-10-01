package backup

import (
	"context"
	"time"

	"go.uber.org/zap"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/redis/go-redis/v9"
)

// schedulerLockPrefix keys the Redis lease that runs each scheduler tick on one replica.
const schedulerLockPrefix = "notifications:backup:tick"

// SchedulerConfig configures the hourly auto-backup + retention churn.
type SchedulerConfig struct {
	Enabled       bool // BACKUP_SCHEDULE_ENABLED (default true)
	Hour          int  // BACKUP_SCHEDULE_HOUR (default 2) — service-local time (legacy/global; per-tenant hour wins)
	RetentionDays int  // BACKUP_RETENTION_DAYS (default 4) — safety churn window
}

// Scheduler wakes at every top of the hour and backs up ONLY tenants that have opted in
// (auto_enabled=true) AND scheduled that hour. Auto-backup is OPT-IN: tenants that have not
// activated it are never touched. A Redis once-per-hour lease ensures only one replica works.
type Scheduler struct {
	rdb redis.UniversalClient
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

// runGuarded runs one tick on a single replica. Hourly ticks run once per hour fleet-wide
// (RunOnce keyed by the hour, so a replica whose timer fires a moment later cannot repeat
// it); the startup churn (backupHour -1) only needs mutual exclusion. This replaced a
// session pg_try_advisory_lock, which PgBouncer transaction pooling breaks (lock and unlock
// can land on different server connections, leaking the lock or admitting a second replica).
func (sc *Scheduler) runGuarded(ctx context.Context, backupHour int) {
	tick := func(ctx context.Context) error { return sc.runTick(ctx, backupHour) }
	var ran bool
	var err error
	if backupHour < 0 {
		ran, err = sharedcache.RunExclusive(ctx, sc.rdb, sc.log, schedulerLockPrefix+":startup", 30*time.Minute, tick)
	} else {
		ran, err = sharedcache.RunOnce(ctx, sc.rdb, sc.log, sharedcache.PeriodKey(schedulerLockPrefix, time.Hour), time.Hour, tick)
	}
	if err != nil {
		sc.log.Warn("scheduler: tick not run", zap.Bool("ran", ran), zap.Error(err))
	}
}

// runTick backs up the tenants that activated auto-backup for backupHour (when >= 0), then
// runs the retention churn.
func (sc *Scheduler) runTick(ctx context.Context, backupHour int) error {
	if backupHour >= 0 {
		sc.backupActivatedTenants(ctx, backupHour)
	}
	if _, err := sc.svc.Churn(ctx, sc.cfg.RetentionDays); err != nil {
		sc.log.Warn("scheduler: churn failed", zap.Error(err))
	}
	return nil
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

// WithRedis sets the Redis client used for the cross-replica tick lease. Without it no tick
// runs (logged), because running on every replica would duplicate backups.
func (sc *Scheduler) WithRedis(rdb redis.UniversalClient) *Scheduler {
	sc.rdb = rdb
	return sc
}
