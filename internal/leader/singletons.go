package leader

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Singleton is a background service that must run on exactly one Hub replica
// at a time (pollers, reapers, retention, evaluators). Run blocks until ctx is
// cancelled; it is invoked again on every leadership acquisition, so it must
// construct any single-shot state (channels, tickers) inside itself.
type Singleton struct {
	Name string
	Run  func(ctx context.Context)
}

// Singletons starts a set of Singleton services when leadership is acquired
// and stops them (cancelling their context and waiting) when it is lost.
type Singletons struct {
	services []Singleton
	stopWait time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     *sync.WaitGroup
}

// NewSingletons groups services for one elector.
func NewSingletons(services ...Singleton) *Singletons {
	return &Singletons{services: services, stopWait: 10 * time.Second}
}

// Add appends a service; call before Start.
func (s *Singletons) Add(name string, run func(ctx context.Context)) {
	s.services = append(s.services, Singleton{Name: name, Run: run})
}

// Start launches every service under a context derived from parent. Calling
// Start while already running is a no-op.
func (s *Singletons) Start(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	wg := &sync.WaitGroup{}
	s.wg = wg
	for _, svc := range s.services {
		wg.Add(1)
		go func(svc Singleton) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("leader: singleton panicked", "name", svc.Name, "panic", r)
				}
			}()
			slog.Info("leader: singleton started", "name", svc.Name)
			svc.Run(ctx)
			slog.Info("leader: singleton stopped", "name", svc.Name)
		}(svc)
	}
}

// Stop cancels the services and waits up to stopWait for them to return.
func (s *Singletons) Stop() {
	s.mu.Lock()
	cancel, wg := s.cancel, s.wg
	s.cancel, s.wg = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(s.stopWait):
		slog.Warn("leader: singletons did not stop in time", "wait", s.stopWait)
	}
}

// Running reports whether the services are currently started.
func (s *Singletons) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel != nil
}

// RunWith drives Singletons from an elector: services start on acquisition
// and stop on loss; extra callbacks run after start / before stop. Blocks
// until ctx is cancelled.
func RunWith(ctx context.Context, l Leader, s *Singletons, onAcquired, onLost func()) {
	l.Run(ctx, func() {
		s.Start(ctx)
		if onAcquired != nil {
			onAcquired()
		}
	}, func() {
		if onLost != nil {
			onLost()
		}
		s.Stop()
	})
}
