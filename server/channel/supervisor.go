package channel

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// DefaultReconcileInterval is how often a Supervisor re-reads the account list.
//
// Connecting and disconnecting accounts is rare, and polling avoids a second message path between the
// HTTP handlers and the receive loops. The cost is that a newly connected account may wait up to one
// interval before messages come in; the connect flow itself (scan a QR code / approve on a page) takes
// longer than that.
const DefaultReconcileInterval = time.Minute

// Account is one connected account of a channel (a Feishu app, a WeChat bot).
type Account struct {
	// Key identifies the account; one receive loop runs per key.
	Key string
	// Version changes when the loop must restart with new settings (for example a new token after the
	// user logged in again). Accounts with the same Key and Version keep their running loop.
	Version string
	// Value is the channel's own account data, handed back to RunAccount.
	Value any
}

// Source is what a Supervisor needs from a channel.
type Source interface {
	// Name is used in logs ("feishu", "weixin").
	Name() string
	// ListAccounts returns every account that should have a running receive loop.
	ListAccounts(ctx context.Context) ([]Account, error)
	// RunAccount runs one account's receive loop and blocks until ctx is done or the loop gives up.
	// When it returns, the account is started again at the next reconcile.
	RunAccount(ctx context.Context, account Account) error
}

// Supervisor keeps one receive loop running for every account of a Source.
//
//	start                 start a loop for every account
//	every interval        start loops for new accounts, stop loops for removed or changed ones
//	ctx done              stop everything and wait, so leases are given back before the process exits
//
// The loops themselves decide which instance actually connects (see RunWithLease); the Supervisor
// runs on every instance.
type Supervisor struct {
	src      Source
	log      *slog.Logger
	interval time.Duration

	mu      sync.Mutex
	running map[string]*runningAccount
	wg      sync.WaitGroup
}

type runningAccount struct {
	version string
	cancel  context.CancelFunc
}

// NewSupervisor creates a supervisor for src. interval <= 0 uses DefaultReconcileInterval.
func NewSupervisor(src Source, interval time.Duration, log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	return &Supervisor{
		src:      src,
		log:      log.With("channel", src.Name()),
		interval: interval,
		running:  make(map[string]*runningAccount),
	}
}

// Run blocks until ctx is done, then waits for every loop to exit.
func (s *Supervisor) Run(ctx context.Context) error {
	s.Reconcile(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return nil
		case <-ticker.C:
			s.Reconcile(ctx)
		}
	}
}

// Reconcile compares the running loops with the account list once.
func (s *Supervisor) Reconcile(ctx context.Context) {
	accounts, err := s.src.ListAccounts(ctx)
	if err != nil {
		s.log.Warn("列账号失败，这轮对账跳过", "error", err.Error())
		return
	}
	want := make(map[string]Account, len(accounts))
	for _, a := range accounts {
		if key := strings.TrimSpace(a.Key); key != "" {
			a.Key = key
			want[key] = a
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for key, r := range s.running {
		a, ok := want[key]
		switch {
		case !ok:
			s.log.Info("账号已断开，停掉它的接收循环", "account", key)
		case a.Version != r.version:
			s.log.Info("账号配置变了，重启它的接收循环", "account", key)
		default:
			continue
		}
		r.cancel()
		delete(s.running, key)
	}
	for key, a := range want {
		if _, ok := s.running[key]; ok {
			continue
		}
		s.startLocked(ctx, a)
	}
}

// Running reports how many loops are running (for tests and logs).
func (s *Supervisor) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

// startLocked starts one loop. The caller holds s.mu; nothing remote happens under the lock.
func (s *Supervisor) startLocked(ctx context.Context, a Account) {
	runCtx, cancel := context.WithCancel(ctx)
	r := &runningAccount{version: a.Version, cancel: cancel}
	s.running[a.Key] = r
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer func() {
			// Forget it so the next reconcile starts it again. Only remove our own entry: a restart for a
			// new version may already have put a newer one in place.
			s.mu.Lock()
			if s.running[a.Key] == r {
				delete(s.running, a.Key)
			}
			s.mu.Unlock()
		}()
		if err := s.src.RunAccount(runCtx, a); err != nil && runCtx.Err() == nil {
			s.log.Error("接收循环退出", "account", a.Key, "error", err.Error())
		}
	}()
}
