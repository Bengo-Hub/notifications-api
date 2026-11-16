package main

import (
	"context"
	"time"

	"github.com/Bengo-Hub/httpware/pii"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bengobox/notifications-api/internal/providers/whatsapp"
)

// unreachableQuery finds a live 131026 suppression of a number for the sender whose number sends
// it. The webhook records the sender as the owner of the receiving phone_number_id: the tenant's
// id, or null (or the platform's id) for the platform number, so the platform matches either.
const unreachableQuery = `SELECT 1 FROM suppressions
WHERE channel = 'whatsapp' AND scope = 'all' AND reason = 'unreachable'
  AND expires_at > $1 AND address_hash = $2
  AND (tenant_id = $3 OR ($4 AND tenant_id IS NULL))
LIMIT 1`

// whatsAppUnreachable reports whether Meta said, within the last week, that it cannot deliver to
// this number for this sender (131026). Any lookup failure answers false, so a send is never
// lost to it.
func whatsAppUnreachable(ctx context.Context, pool *pgxpool.Pool, senderTenantID, platformID, to string, metadata map[string]any) bool {
	if pool == nil || to == "" {
		return false
	}
	dial, _ := metadata["default_dial_code"].(string)
	hash := pii.HashAddress("+" + whatsapp.InternationalNumber(to, dial))
	sender, err := uuid.Parse(senderTenantID)
	if err != nil {
		sender = uuid.Nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	err = pool.QueryRow(ctx, unreachableQuery, time.Now(), hash, sender, senderTenantID == platformID).Scan(&one)
	return err == nil && one == 1
}
