package backup

import (
	"context"
	"os"
	"time"

	"github.com/google/uuid"

	entbackup "github.com/bengobox/notifications-api/internal/ent/backup"
	entbackupsetting "github.com/bengobox/notifications-api/internal/ent/backupsetting"
)

type Settings struct {
	AutoEnabled   bool `json:"auto_enabled"`
	ScheduleHour  int  `json:"schedule_hour"`
	RetentionDays int  `json:"retention_days"`
}

func DefaultSettings() Settings {
	return Settings{AutoEnabled: false, ScheduleHour: 2, RetentionDays: DefaultRetentionDays}
}

func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (Settings, error) {
	row, err := s.orm.BackupSetting.Query().Where(entbackupsetting.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return DefaultSettings(), nil //nolint:nilerr
	}
	return Settings{AutoEnabled: row.AutoEnabled, ScheduleHour: row.ScheduleHour, RetentionDays: row.RetentionDays}, nil
}

func (s *Service) UpsertSettings(ctx context.Context, tenantID uuid.UUID, in Settings) (Settings, error) {
	if in.ScheduleHour < 0 || in.ScheduleHour > 23 {
		in.ScheduleHour = 2
	}
	if in.RetentionDays <= 0 {
		in.RetentionDays = DefaultRetentionDays
	}
	existing, err := s.orm.BackupSetting.Query().Where(entbackupsetting.TenantID(tenantID)).Only(ctx)
	if err == nil {
		_, err = existing.Update().SetAutoEnabled(in.AutoEnabled).SetScheduleHour(in.ScheduleHour).SetRetentionDays(in.RetentionDays).Save(ctx)
		return in, err
	}
	_, err = s.orm.BackupSetting.Create().SetTenantID(tenantID).SetAutoEnabled(in.AutoEnabled).SetScheduleHour(in.ScheduleHour).SetRetentionDays(in.RetentionDays).Save(ctx)
	return in, err
}

func (s *Service) ListActivatedTenants(ctx context.Context, hour int) ([]struct {
	TenantID      uuid.UUID
	RetentionDays int
}, error) {
	rows, err := s.orm.BackupSetting.Query().Where(entbackupsetting.AutoEnabled(true), entbackupsetting.ScheduleHour(hour)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]struct {
		TenantID      uuid.UUID
		RetentionDays int
	}, 0, len(rows))
	for _, r := range rows {
		rd := r.RetentionDays
		if rd <= 0 {
			rd = DefaultRetentionDays
		}
		out = append(out, struct {
			TenantID      uuid.UUID
			RetentionDays int
		}{TenantID: r.TenantID, RetentionDays: rd})
	}
	return out, nil
}

func (s *Service) ChurnTenant(ctx context.Context, tenantID uuid.UUID, retentionDays int) (int, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	rows, err := s.orm.Backup.Query().Where(entbackup.TenantID(tenantID), entbackup.CreatedAtLT(cutoff)).All(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, r := range rows {
		if err := os.Remove(r.Path); err != nil && !os.IsNotExist(err) {
			continue
		}
		if err := s.orm.Backup.DeleteOne(r).Exec(ctx); err == nil {
			removed++
		}
	}
	return removed, nil
}
