// Package notify sends notifications about finished OTA batches and failed
// backups (M2). Implementations: webhook (e.g. a Home Assistant webhook
// trigger), and mail via the Resend HTTP API. Both optional and off by default.
package notify

import "context"

// Notifier delivers one message.
type Notifier interface {
	Notify(ctx context.Context, title, body string) error
}
