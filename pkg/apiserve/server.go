package apiserve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// Server hosts the HTTP API that lets other services drive an AgentLoop.
type Server struct {
	settings ServerSettings
	cfg      *config.Config
	bus      *bus.MessageBus
	loop     *agent.AgentLoop
	store    *Store
	gate     *Gate
	tasks    *TaskRunner
	limiter  *RateLimiter

	// accessLog is the injectable sink for structured access records; nil means
	// write them through pkg/logger. Tests replace it to assert on the fields.
	accessLog func(AccessRecord)

	mux     *http.ServeMux
	httpSrv *http.Server
	ready   atomic.Bool
	stopped sync.Once

	// listener is the pre-bound listener supplied through Options.Listener, if
	// any. When nil, Start binds Settings.Host:Settings.Port itself.
	listener net.Listener
}

// New assembles the runtime: config -> provider -> AgentLoop -> HTTP server.
//
// The message bus is only required because agent.NewAgentLoop takes one; this
// service never consumes inbound or outbound messages.
func New(opts Options) (*Server, error) {
	home := opts.Home
	if home == "" {
		home = ResolveHome()
	}

	cfg := opts.Config
	if cfg == nil {
		cfgPath := opts.ConfigPath
		if cfgPath == "" {
			// Resolve from the effective home: ResolveConfigPath() resolves the
			// home itself from the environment, which would load the default
			// home's config while the state goes to opts.Home.
			cfgPath = ResolveConfigPathFor(home)
		}
		loaded, err := config.LoadConfig(cfgPath)
		if err != nil {
			return nil, fmt.Errorf("load config: %w", err)
		}
		cfg = loaded
	}

	provider := opts.Provider
	if provider == nil {
		p, modelID, err := providers.CreateProvider(cfg)
		if err != nil {
			return nil, fmt.Errorf("create provider: %w", err)
		}
		if modelID != "" {
			cfg.Agents.Defaults.ModelName = modelID
		}
		provider = p
	}

	settings := DefaultSettings(home)
	if opts.Settings != nil {
		settings = mergeSettings(settings, *opts.Settings)
	}

	store, err := OpenStore(settings.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	metered := NewMeteredProvider(provider, store)

	msgBus := bus.NewMessageBus()
	loop := agent.NewAgentLoop(cfg, msgBus, metered)
	msgBus.SetEventPublisher(loop.RuntimeEventBus())

	srv := &Server{
		settings: settings,
		cfg:      cfg,
		bus:      msgBus,
		loop:     loop,
		store:    store,
		gate:     NewGate(),
		limiter:  NewRateLimiter(),
		mux:      http.NewServeMux(),
		listener: opts.Listener,
	}
	srv.tasks = NewTaskRunner(srv)
	srv.registerRoutes()
	srv.httpSrv = &http.Server{
		Addr:    net.JoinHostPort(srv.settings.Host, strconv.Itoa(srv.settings.Port)),
		Handler: srv.mux,
	}
	return srv, nil
}

// Handler exposes the router (used by tests).
func (s *Server) Handler() http.Handler { return s.mux }

// SetReady flips the /ready probe, mirroring the gateway's readiness gate.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// Start serves HTTP until ctx is cancelled, then shuts down.
func (s *Server) Start(ctx context.Context) error {
	ln := s.listener
	if ln == nil {
		bound, err := net.Listen("tcp", s.httpSrv.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", s.httpSrv.Addr, err)
		}
		ln = bound
	}
	s.ready.Store(true)

	// The async task pool must be running in production, not only in tests: the
	// poller it starts is the only thing that claims queued tasks.
	s.tasks.Start(ctx)

	logger.InfoC("apiserve", fmt.Sprintf("API server listening on %s", ln.Addr().String()))

	errCh := make(chan error, 1)
	go func() { errCh <- s.httpSrv.Serve(ln) }()

	select {
	case <-ctx.Done():
		return s.Shutdown(context.Background())
	case serveErr := <-errCh:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		// Serve failed *after* the pool was started. Shut down here too: without
		// it the workers, the AgentLoop and the store all leak for the life of
		// the process, and the caller thinks Start merely returned an error.
		_ = s.Shutdown(context.Background())
		return serveErr
	}
}

// Shutdown releases the listener and the embedded AgentLoop exactly once.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.stopped.Do(func() {
		s.ready.Store(false)
		err = s.httpSrv.Shutdown(ctx)
		s.tasks.Stop()
		s.loop.Stop()
		s.loop.Close()
		s.bus.Close()
		if closeErr := s.store.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	})
	return err
}
