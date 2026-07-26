package core

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

// Phase is the coarse lifecycle state of the service, shown as the header
// status pill in the GUI.
type Phase int

const (
	PhaseIdle Phase = iota
	PhaseStarting
	PhaseReady
	PhaseRetrying
	PhaseError
	PhaseStopped
)

func (p Phase) String() string {
	switch p {
	case PhaseStarting:
		return "starting"
	case PhaseReady:
		return "ready"
	case PhaseRetrying:
		return "retrying"
	case PhaseError:
		return "error"
	case PhaseStopped:
		return "stopped"
	default:
		return "idle"
	}
}

// StepState is the state of one boot step.
type StepState int

const (
	StepPending StepState = iota
	StepRunning
	StepDone
	StepFailed
	StepSkipped
)

// Boot step keys. The GUI maps these onto localised titles.
const (
	StepKeyConfig   = "config"
	StepKeyTsnet    = "tsnet"
	StepKeyRules    = "rules"
	StepKeyServices = "services"
	StepKeyMonitors = "monitors"
	StepKeyReady    = "ready"
)

// BootStep is one entry in the startup checklist.
type BootStep struct {
	Key      string
	State    StepState
	Err      string
	Started  time.Time
	Finished time.Time
}

// Elapsed is how long the step took, or how long it has been running.
func (s BootStep) Elapsed() time.Duration {
	if s.Started.IsZero() {
		return 0
	}
	if s.Finished.IsZero() {
		return time.Since(s.Started)
	}
	return s.Finished.Sub(s.Started)
}

// State is an immutable snapshot of the supervisor, safe to read from the UI
// goroutine.
type State struct {
	Phase     Phase
	Steps     []BootStep
	Err       string
	StartedAt time.Time
	ReadyAt   time.Time
	Restarts  int
	// NextRetryAt is set while Phase is PhaseRetrying.
	NextRetryAt time.Time

	Config *Config
	Server *tsnet.Server
	Peers  *PeerMonitor
	Lan    *LanScanner
}

// Ready reports whether the service finished booting.
func (s State) Ready() bool { return s.Phase == PhaseReady }

// Progress is the fraction of boot steps completed, for the splash bar.
func (s State) Progress() float32 {
	if len(s.Steps) == 0 {
		return 0
	}
	done := 0
	for _, st := range s.Steps {
		if st.State == StepDone || st.State == StepSkipped {
			done++
		}
	}
	return float32(done) / float32(len(s.Steps))
}

// SupervisorOptions configures a Supervisor.
type SupervisorOptions struct {
	ConfigPath string
	ConfigURL  string
	TsnetDebug bool
	Logger     *slog.Logger
	// MaxBackoff caps the retry delay. Zero means 30s.
	MaxBackoff time.Duration
}

// Supervisor owns the service lifecycle for the GUI. It is the same startup
// sequence the headless binary runs in serviceLogic, split into observable
// steps and wrapped in a restart loop that keeps the window alive when
// tailscale is unreachable — a CLI can exit on failure, a GUI must explain
// itself instead.
type Supervisor struct {
	opt    SupervisorOptions
	logger *slog.Logger

	mu    sync.RWMutex
	state State

	subsMu  sync.Mutex
	subs    map[int]chan struct{}
	nextSub int

	restartCh chan struct{}
	stopOnce  sync.Once
}

// NewSupervisor creates an unstarted supervisor.
func NewSupervisor(opt SupervisorOptions) *Supervisor {
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opt.MaxBackoff <= 0 {
		opt.MaxBackoff = 30 * time.Second
	}
	return &Supervisor{
		opt:       opt,
		logger:    logger.With("from", "supervisor"),
		subs:      make(map[int]chan struct{}),
		restartCh: make(chan struct{}, 1),
		state: State{
			Phase: PhaseIdle,
			Steps: freshSteps(),
		},
	}
}

func freshSteps() []BootStep {
	keys := []string{
		StepKeyConfig, StepKeyTsnet, StepKeyRules,
		StepKeyServices, StepKeyMonitors, StepKeyReady,
	}
	steps := make([]BootStep, len(keys))
	for i, k := range keys {
		steps[i] = BootStep{Key: k}
	}
	return steps
}

// Snapshot returns the current state.
func (s *Supervisor) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.state
	st.Steps = append([]BootStep(nil), s.state.Steps...)
	return st
}

// Subscribe returns a coalescing wakeup channel and a cancel func.
func (s *Supervisor) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.subsMu.Lock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = ch
	s.subsMu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.subsMu.Lock()
			delete(s.subs, id)
			s.subsMu.Unlock()
		})
	}
}

func (s *Supervisor) notify() {
	s.subsMu.Lock()
	for _, ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	s.subsMu.Unlock()
}

func (s *Supervisor) update(f func(*State)) {
	s.mu.Lock()
	f(&s.state)
	s.mu.Unlock()
	s.notify()
}

func (s *Supervisor) stepStart(key string) {
	s.update(func(st *State) {
		for i := range st.Steps {
			if st.Steps[i].Key == key {
				st.Steps[i].State = StepRunning
				st.Steps[i].Started = time.Now()
				st.Steps[i].Err = ""
				return
			}
		}
	})
}

func (s *Supervisor) stepDone(key string, err error) {
	s.update(func(st *State) {
		for i := range st.Steps {
			if st.Steps[i].Key != key {
				continue
			}
			st.Steps[i].Finished = time.Now()
			if err != nil {
				st.Steps[i].State = StepFailed
				st.Steps[i].Err = err.Error()
			} else {
				st.Steps[i].State = StepDone
			}
			return
		}
	})
}

// Restart asks the supervisor to tear down and boot again. It never blocks.
func (s *Supervisor) Restart() {
	select {
	case s.restartCh <- struct{}{}:
	default:
	}
}

// Run drives the boot-and-supervise loop until ctx is cancelled. It blocks, so
// callers run it on their own goroutine.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			s.update(func(st *State) { st.Phase = PhaseStopped })
			return
		}

		runCtx, cancel := context.WithCancel(ctx)
		err := s.boot(runCtx)
		if err == nil {
			backoff = time.Second
			// Supervise until something asks us to restart.
			reason := s.supervise(runCtx)
			cancel()
			s.teardown()
			if ctx.Err() != nil {
				s.update(func(st *State) { st.Phase = PhaseStopped })
				return
			}
			s.logger.Warn("restarting service", "reason", reason)
			s.update(func(st *State) {
				st.Phase = PhaseRetrying
				st.Restarts++
				st.Steps = freshSteps()
				st.NextRetryAt = time.Now().Add(time.Second)
			})
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}

		cancel()
		s.teardown()
		if ctx.Err() != nil {
			s.update(func(st *State) { st.Phase = PhaseStopped })
			return
		}

		// Configuration errors will not fix themselves; surface them and wait
		// for an explicit Restart rather than looping on a broken file.
		if errors.Is(err, errFatalConfig) {
			// Log it as well as showing it: the on-screen log sheet is the
			// thing users screenshot, and a bare error panel with an empty log
			// tells whoever is helping them nothing.
			s.logger.Error("configuration error, waiting for retry", "err", err)
			s.update(func(st *State) {
				st.Phase = PhaseError
				st.Err = err.Error()
			})
			select {
			case <-ctx.Done():
				s.update(func(st *State) { st.Phase = PhaseStopped })
				return
			case <-s.restartCh:
				s.update(func(st *State) {
					st.Phase = PhaseStarting
					st.Err = ""
					st.Steps = freshSteps()
				})
				continue
			}
		}

		s.logger.Warn("startup failed, retrying", "err", err, "backoff", backoff)
		s.update(func(st *State) {
			st.Phase = PhaseRetrying
			st.Err = err.Error()
			st.Restarts++
			st.NextRetryAt = time.Now().Add(backoff)
		})
		select {
		case <-ctx.Done():
			s.update(func(st *State) { st.Phase = PhaseStopped })
			return
		case <-s.restartCh:
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > s.opt.MaxBackoff {
			backoff = s.opt.MaxBackoff
		}
		s.update(func(st *State) { st.Steps = freshSteps() })
	}
}

// errFatalConfig marks an error that retrying cannot fix.
var errFatalConfig = errors.New("configuration error")

// boot runs the startup sequence, reporting each step.
func (s *Supervisor) boot(ctx context.Context) error {
	s.update(func(st *State) {
		st.Phase = PhaseStarting
		st.Err = ""
		st.StartedAt = time.Now()
		st.ReadyAt = time.Time{}
		st.NextRetryAt = time.Time{}
	})

	// --- config -----------------------------------------------------------
	s.stepStart(StepKeyConfig)
	source := s.opt.ConfigPath
	if s.opt.ConfigURL != "" {
		source = s.opt.ConfigURL
		s.logger.Info("using config url", "url", s.opt.ConfigURL)
	}
	cfg, err := LoadConfig(source)
	if err != nil {
		s.logger.Error("failed to load config", "source", source, "err", err)
		s.stepDone(StepKeyConfig, err)
		return errors.Join(errFatalConfig, err)
	}
	SetDoHServers(cfg.DNS.DoHServers)
	if len(cfg.DNS.DoHServers) > 0 {
		s.logger.Info("dns-over-https fallback enabled", "servers", cfg.DNS.DoHServers)
	}
	s.update(func(st *State) { st.Config = cfg })
	s.stepDone(StepKeyConfig, nil)

	// --- tsnet ------------------------------------------------------------
	s.stepStart(StepKeyTsnet)
	srv, err := InitTsNet(ctx, &cfg.Core, s.logger, s.opt.TsnetDebug)
	if err != nil {
		s.stepDone(StepKeyTsnet, err)
		return err
	}
	s.update(func(st *State) { st.Server = srv })
	s.stepDone(StepKeyTsnet, nil)

	// --- rules ------------------------------------------------------------
	s.stepStart(StepKeyRules)
	NormalizeConnectRulesDstAddr(ctx, srv, cfg.Connect, s.logger)
	s.stepDone(StepKeyRules, nil)

	// --- services ---------------------------------------------------------
	s.stepStart(StepKeyServices)
	StartForwarders(ctx, srv, cfg.Forward)
	StartConnectors(ctx, srv, cfg.Connect)
	RunLanDiscoverService(ctx, cfg.Connect, s.logger.With("from", "lan_service"))
	s.stepDone(StepKeyServices, nil)

	// --- monitors ---------------------------------------------------------
	s.stepStart(StepKeyMonitors)
	peers := NewPeerMonitor(srv, cfg.Connect, s.logger, PeerMonitorOptions{})
	peers.Start(ctx)

	lan := NewLanScanner(s.logger.With("from", "lan_scan"))
	lan.SetSelfEntries(LanEntriesFromRules(cfg.Connect))
	lan.Start(ctx)

	s.update(func(st *State) {
		st.Peers = peers
		st.Lan = lan
	})
	s.stepDone(StepKeyMonitors, nil)

	// --- ready ------------------------------------------------------------
	s.stepStart(StepKeyReady)
	s.stepDone(StepKeyReady, nil)
	s.update(func(st *State) {
		st.Phase = PhaseReady
		st.ReadyAt = time.Now()
		st.Err = ""
	})
	s.logger.Info("service ready", "took", time.Since(s.Snapshot().StartedAt).Round(time.Millisecond))
	return nil
}

// supervise blocks until the service should be restarted, returning why.
func (s *Supervisor) supervise(ctx context.Context) string {
	watchdog := StartTimeWatchDog(ctx, s.logger.With("from", "watchdog"))
	for {
		select {
		case <-ctx.Done():
			return "context cancelled"
		case <-watchdog:
			return "system time jump"
		case <-s.restartCh:
			return "requested by user"
		}
	}
}

// teardown closes the tsnet server and clears the per-run state.
func (s *Supervisor) teardown() {
	s.mu.Lock()
	srv := s.state.Server
	s.state.Server = nil
	s.state.Peers = nil
	s.state.Lan = nil
	s.mu.Unlock()

	if srv != nil {
		if err := srv.Close(); err != nil {
			s.logger.Debug("closing tsnet server", "err", err)
		}
	}
	s.notify()
}
