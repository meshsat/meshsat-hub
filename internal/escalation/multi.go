package escalation

import (
	"context"
	"fmt"
	"strings"
)

// MultiNotifier fans out notifications to multiple backends.
// All backends are called; errors are collected and returned as one.
type MultiNotifier struct {
	backends []Notifier
}

// NewMultiNotifier creates a notifier that sends to all backends.
func NewMultiNotifier(backends ...Notifier) *MultiNotifier {
	return &MultiNotifier{backends: backends}
}

func (m *MultiNotifier) Notify(ctx context.Context, targets []string, subject, body string) error {
	var errs []string
	for _, b := range m.backends {
		if err := b.Notify(ctx, targets, subject, body); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("notify: %s", strings.Join(errs, "; "))
	}
	return nil
}
