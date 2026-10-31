package messaging

import (
	"context"
	"sync/atomic"
)

// ChannelGate answers why a message must not be queued at all, or "" when it may be. It is the
// source-side check for paid channels: a tenant with no SMS or WhatsApp plan, bundle or credit
// never gets a message queued, rendered or logged for that channel. The worker's charge at send
// time stays the final word for the rare case a balance runs out in between.
type ChannelGate func(ctx context.Context, msg Message) (blockedReason string)

var channelGate atomic.Pointer[ChannelGate]

// SetChannelGate installs the gate every Publish consults. Each binary (API, worker) sets it once
// at startup; nil removes it.
func SetChannelGate(g ChannelGate) {
	if g == nil {
		channelGate.Store(nil)
		return
	}
	channelGate.Store(&g)
}

// BlockedReason reports why msg would be dropped by the installed gate, "" when it would be queued.
// Callers that track each recipient (broadcasts) use it to record the reason themselves.
func BlockedReason(ctx context.Context, msg Message) string {
	g := channelGate.Load()
	if g == nil {
		return ""
	}
	return (*g)(ctx, msg)
}
