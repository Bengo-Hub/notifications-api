package whatsappinbox

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// StreamMessage is the envelope pushed to inbox WebSocket clients.
type StreamMessage struct {
	Type    string `json:"type"` // "whatsapp_message" | "ping" | "pong"
	Payload any    `json:"payload,omitempty"`
}

type wsClient struct {
	conn     *websocket.Conn
	tenantID uuid.UUID
	send     chan StreamMessage
}

const (
	relayTopic   = "whatsapp_inbox"
	sendBuffer   = 32
	writeTimeout = 5 * time.Second
	pingInterval = 25 * time.Second // under typical 60s proxy idle timeouts
)

// Hub manages active WhatsApp-inbox WebSocket connections, broadcasting tenant-wide (no
// per-assignee targeting, matching the tenant-wide inbox RBAC scoping).
//
// Clients connect to whichever replica the load balancer picks, so a broadcast is relayed to
// every replica through the shared events.Broadcaster (core NATS fan-out); each replica then
// delivers to its own sockets. Clients are indexed by tenant so a broadcast touches only that
// tenant's sockets.
type Hub struct {
	mu      sync.RWMutex
	clients map[uuid.UUID]map[*wsClient]struct{}
	log     *zap.Logger
	relay   *eventslib.Broadcaster
}

func NewHub(log *zap.Logger) *Hub {
	return &Hub{
		clients: make(map[uuid.UUID]map[*wsClient]struct{}),
		log:     log.Named("whatsapp-inbox.hub"),
	}
}

// SetRelay wires the cross-replica relay. A nil relay degrades to single-pod delivery.
func (h *Hub) SetRelay(b *eventslib.Broadcaster) {
	h.relay = b
	if b == nil {
		return
	}
	if err := b.Subscribe(relayTopic, func(m eventslib.BroadcastMessage) {
		tenantID, err := uuid.Parse(m.TenantID)
		if err != nil {
			return
		}
		var msg StreamMessage
		if err := json.Unmarshal(m.Data, &msg); err != nil {
			h.log.Warn("whatsapp-inbox.hub: bad relay message", zap.Error(err))
			return
		}
		h.sendLocal(tenantID, msg)
	}); err != nil {
		h.log.Warn("whatsapp-inbox.hub: relay subscribe failed, single-pod delivery only", zap.Error(err))
	}
}

// ServeWS serves one upgraded connection and blocks until the client disconnects.
func (h *Hub) ServeWS(ctx context.Context, conn *websocket.Conn, tenantID uuid.UUID) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := &wsClient{conn: conn, tenantID: tenantID, send: make(chan StreamMessage, sendBuffer)}

	h.mu.Lock()
	if h.clients[tenantID] == nil {
		h.clients[tenantID] = make(map[*wsClient]struct{})
	}
	h.clients[tenantID][c] = struct{}{}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients[tenantID], c)
		if len(h.clients[tenantID]) == 0 {
			delete(h.clients, tenantID)
		}
		close(c.send)
		h.mu.Unlock()
	}()

	// Writer: every write has a deadline, and the server pings on an interval, so a stalled
	// or vanished client is dropped instead of holding a goroutine and a socket open.
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case msg, ok := <-c.send:
				if !ok {
					return
				}
				wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
				err := wsjson.Write(wctx, conn, msg)
				wcancel()
				if err != nil {
					cancel()
					return
				}
			case <-ticker.C:
				pctx, pcancel := context.WithTimeout(ctx, writeTimeout)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	c.send <- StreamMessage{Type: "ping", Payload: map[string]any{"ts": time.Now().Unix()}}

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var m map[string]any
		if jsonErr := json.Unmarshal(raw, &m); jsonErr == nil {
			if t, _ := m["type"].(string); t == "ping" {
				select {
				case c.send <- StreamMessage{Type: "pong"}:
				default:
				}
			}
		}
	}
}

// BroadcastToTenant delivers msg to every active session for tenantID on every replica.
func (h *Hub) BroadcastToTenant(tenantID uuid.UUID, msg StreamMessage) {
	if h.relay == nil {
		h.sendLocal(tenantID, msg)
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		h.log.Warn("whatsapp-inbox.hub: marshal failed", zap.Error(err))
		return
	}
	// Publish delivers to this replica's handler first, then relays to the others.
	_ = h.relay.Publish(relayTopic, tenantID.String(), "", data)
}

func (h *Hub) sendLocal(tenantID uuid.UUID, msg StreamMessage) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients[tenantID] {
		select {
		case c.send <- msg:
		default:
			h.log.Warn("whatsapp-inbox.hub: send buffer full, dropping broadcast", zap.Stringer("tenant_id", tenantID))
		}
	}
}
