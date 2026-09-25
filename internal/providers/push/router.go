package push

import (
	"context"
	"errors"
	"fmt"
)

// Router sends each device through the channel it registered with: FCM registration tokens via
// Firebase (when a project is configured), browser PushSubscriptions via Web Push. A tenant that
// moves between the two never strands devices registered under the other.
type Router struct {
	FCM *FCMProvider     // nil when no Firebase project is configured
	Web *WebPushProvider // nil when no VAPID keys are available
}

func (r *Router) Name() string {
	switch {
	case r.FCM != nil && r.Web != nil:
		return "fcm+webpush"
	case r.FCM != nil:
		return "fcm"
	default:
		return "webpush"
	}
}

// SendPush splits tokens by kind and sends each group; unregistered devices from either sender are
// reported together. Tokens with no configured sender are left alone (not marked dead).
func (r *Router) SendPush(ctx context.Context, tokens []string, title, body string, data map[string]string) error {
	var fcmTokens, webTokens []string
	for _, t := range tokens {
		if IsWebPushSubscription(t) {
			webTokens = append(webTokens, t)
		} else {
			fcmTokens = append(fcmTokens, t)
		}
	}

	dead := &UnregisteredTokensError{}
	var errs []error
	sent := 0
	collect := func(n int, err error) {
		var u *UnregisteredTokensError
		switch {
		case err == nil:
			sent += n
		case errors.As(err, &u):
			dead.Tokens = append(dead.Tokens, u.Tokens...)
			sent += u.Delivered
			if u.Other != nil {
				errs = append(errs, u.Other)
			}
		default:
			errs = append(errs, err)
		}
	}
	if len(fcmTokens) > 0 && r.FCM != nil {
		collect(len(fcmTokens), r.FCM.SendPush(ctx, fcmTokens, title, body, data))
	}
	if len(webTokens) > 0 && r.Web != nil {
		collect(len(webTokens), r.Web.SendPush(ctx, webTokens, title, body, data))
	}

	other := errors.Join(errs...)
	if len(dead.Tokens) > 0 {
		dead.Delivered = sent
		dead.Other = other
		return dead
	}
	if sent == 0 && other == nil && len(tokens) > 0 {
		return fmt.Errorf("push: no sender configured for these devices")
	}
	return other
}

// AccountInfo checks every configured sender.
func (r *Router) AccountInfo(ctx context.Context) (map[string]interface{}, error) {
	info := map[string]interface{}{}
	if r.FCM != nil {
		fi, err := r.FCM.AccountInfo(ctx)
		if err != nil {
			return nil, err
		}
		info["fcm"] = fi
	}
	if r.Web != nil {
		wi, err := r.Web.AccountInfo(ctx)
		if err != nil {
			return nil, err
		}
		info["webpush"] = wi
	}
	return info, nil
}
