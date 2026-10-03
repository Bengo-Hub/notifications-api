package announcements

import (
	"context"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent/announcement"
)

const purgeJobKey = "notifications:announcements:purge"

// PurgeExpired hard deletes every announcement whose run has ended. An announcement with no end
// runs until the platform admin deactivates or deletes it.
func (s *Service) PurgeExpired(ctx context.Context, now time.Time) (int, error) {
	n, err := s.client.Announcement.Delete().
		Where(announcement.EndsAtNotNil(), announcement.EndsAtLTE(now)).
		Exec(ctx)
	if err == nil && n > 0 {
		s.invalidate()
	}
	return n, err
}

// StartPurger deletes expired announcements on start and then hourly, once per hour fleet-wide
// (shared RunOnce). The delete is idempotent, so without Redis every pod simply runs it.
func (s *Service) StartPurger(ctx context.Context, rdb redis.UniversalClient, log *zap.Logger) {
	log = log.Named("announcements.purger")
	run := func(ctx context.Context) error {
		n, err := s.PurgeExpired(ctx, time.Now())
		if err != nil {
			log.Warn("purge expired announcements failed", zap.Error(err))
			return err
		}
		if n > 0 {
			log.Info("purged expired announcements", zap.Int("deleted", n))
		}
		return nil
	}
	tick := func() {
		if rdb == nil {
			_ = run(ctx)
			return
		}
		if _, err := sharedcache.RunOnce(ctx, rdb, log, sharedcache.PeriodKey(purgeJobKey, time.Hour), time.Hour, run); err != nil {
			log.Debug("purge tick not run here", zap.Error(err))
		}
	}
	go func() {
		tick()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tick()
			}
		}
	}()
}
