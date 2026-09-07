package whatsappinbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
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

// Hub manages active WhatsApp-inbox WebSocket connections, broadcasting tenant-wide (no
// per-assignee targeting — matches the tenant-wide inbox RBAC scoping). Mirrors pos-api's
// notifications.Hub pattern (client registry + Redis cross-pod relay + ping/pong keepalive),
// stripped to tenant-only broadcast since there's no per-user targeting need here.
type Hub struct {
	mu       sync.RWMutex
	clients  map[*wsClient]struct{}
	log      *zap.Logger
	redis    *redis.Client
	originID string
}

func NewHub(log *zap.Logger) *Hub {
	return &Hub{
		clients:  make(map[*wsClient]struct{}),
		log:      log.Named("whatsapp-inbox.hub"),
		originID: uuid.NewString(),
	}
}

// SetRedis wires the cross-pod relay. Call before Start. A nil client degrades to single-pod mode.
func (h *Hub) SetRedis(rdb *redis.Client) { h.redis = rdb }

// Start subscribes to the cross-pod relay channel and relays to this pod's local clients. Blocks
// until ctx is cancelled — run in a goroutine.
func (h *Hub) Start(ctx context.Context) {
	if h.redis == nil {
		h.log.Info("whatsapp-inbox.hub: no Redis client — single-pod broadcast only")
		return
	}
	sub := h.redis.PSubscribe(ctx, "whatsapp_inbox:*")
	defer func() { _ = sub.Close() }()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			h.relayFromRedis(msg.Channel, msg.Payload)
		}
	}
}

type relayEnvelope struct {
	Msg    StreamMessage `json:"msg"`
	Origin string        `json:"origin"`
}

func (h *Hub) relayFromRedis(channel, payload string) {
	tenantID, ok := parseChannel(channel)
	if !ok {
		return
	}
	var env relayEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		h.log.Warn("whatsapp-inbox.hub: failed to decode redis relay message", zap.Error(err))
		return
	}
	if env.Origin == h.originID {
		return // avoid double-delivering our own publish to local clients
	}
	h.sendLocal(tenantID, env.Msg)
}

func parseChannel(channel string) (uuid.UUID, bool) {
	parts := strings.SplitN(channel, ":", 2)
	if len(parts) != 2 || parts[0] != "whatsapp_inbox" {
		return uuid.Nil, false
	}
	tid, err := uuid.Parse(parts[1])
	if err != nil {
		return uuid.Nil, false
	}
	return tid, true
}

func (h *Hub) publish(tenantID uuid.UUID, msg StreamMessage) {
	if h.redis == nil {
		return
	}
	payload, err := json.Marshal(relayEnvelope{Msg: msg, Origin: h.originID})
	if err != nil {
		h.log.Warn("whatsapp-inbox.hub: failed to marshal relay payload", zap.Error(err))
		return
	}
	channel := fmt.Sprintf("whatsapp_inbox:%s", tenantID)
	if err := h.redis.Publish(context.Background(), channel, payload).Err(); err != nil {
		h.log.Warn("whatsapp-inbox.hub: redis publish failed", zap.Error(err))
	}
}

// ServeWS upgrades the HTTP connection and blocks until the client disconnects.
func (h *Hub) ServeWS(ctx context.Context, conn *websocket.Conn, tenantID uuid.UUID) {
	c := &wsClient{conn: conn, tenantID: tenantID, send: make(chan StreamMessage, 32)}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		close(c.send)
		h.mu.Unlock()
	}()

	go func() {
		for msg := range c.send {
			if err := wsjson.Write(ctx, conn, msg); err != nil {
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

// BroadcastToTenant delivers msg to every active session for tenantID — locally, and (via Redis)
// on every other replica too.
func (h *Hub) BroadcastToTenant(tenantID uuid.UUID, msg StreamMessage) {
	h.sendLocal(tenantID, msg)
	h.publish(tenantID, msg)
}

func (h *Hub) sendLocal(tenantID uuid.UUID, msg StreamMessage) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if c.tenantID == tenantID {
			select {
			case c.send <- msg:
			default:
				h.log.Warn("whatsapp-inbox.hub: send buffer full, dropping broadcast", zap.Stringer("tenant_id", tenantID))
			}
		}
	}
}
