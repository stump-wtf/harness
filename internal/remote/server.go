// Package remote is the opt-in Wish SSH front door (ADR-0004). Each SSH session
// hosts the SAME internal/tui Bubble Tea Model in-process, wired as an ordinary
// local client of the daemon's framed protocol over the Unix socket — there is
// no separate remote protocol, so a remote attach inherits snapshot-on-attach,
// backpressure, and resize behaviour identically to a local one (SPEC-0002 REQ
// "Transport Bindings", scenario "Remote parity").
//
// Governing: SPEC-0002 (Transport Bindings), ADR-0004 (Wish data plane; remote
// is the same TUI), ADR-0008 (SSH public-key auth only, persistent host key,
// read-only attach scoping, secrets stay in env_file — never in daemon state or
// logs). The server is off unless explicitly enabled.
package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	bm "github.com/charmbracelet/wish/bubbletea"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui"
)

// DefaultListen is the conservative default bind address: loopback only, so an
// enabled server is not exposed to the network without a deliberate address
// (ADR-0008: "bind narrowly by default").
const DefaultListen = "127.0.0.1:2222"

// roContextKey marks, on the SSH session context, that the authenticating key
// is read-only. It is set by the auth handler and read when the session's TUI
// Model is constructed.
type roContextKeyType struct{}

var roContextKey roContextKeyType

// Options configures a Server. Socket/ConfigPath/Version are handed straight to
// the per-session tui.New so a remote session is byte-for-byte the local TUI.
type Options struct {
	// Listen is the SSH bind address (host:port). Empty uses DefaultListen.
	Listen string
	// Socket is the daemon's control/data Unix socket each session dials.
	Socket string
	// ConfigPath is the harness.toml path (for the TUI's config-aware views).
	ConfigPath string
	// Version is the daemon version, surfaced in the TUI handshake.
	Version string
	// HostKeyPath is the persisted SSH host key. Empty uses
	// DefaultHostKeyPath() under $XDG_STATE_HOME/harness (ADR-0008).
	HostKeyPath string
	// Keys and KeysFile are the public-key allowlist sources (ADR-0008).
	Keys     []core.AuthorizedKey
	KeysFile string
}

// DefaultHostKeyPath returns the persisted host-key location,
// $XDG_STATE_HOME/harness/ssh_host_ed25519_key (ADR-0008). Reusing
// supervisor.StateHome keeps every persisted artifact under one 0700 dir.
func DefaultHostKeyPath() string {
	return filepath.Join(supervisor.StateHome(), "ssh_host_ed25519_key")
}

// Server is the opt-in Wish SSH server. It owns a *ssh.Server plus the resolved
// allowlist; Serve/Shutdown drive its lifecycle.
type Server struct {
	ssh  *ssh.Server
	addr string
}

// New builds (but does not start) the SSH server: it resolves the allowlist,
// ensures a persistent host key exists (generated 0600 under a 0700 dir on
// first run, ADR-0008), and wires the Bubble Tea middleware that hosts the TUI.
// It returns an error if the allowlist is empty — an enabled server with no
// authorized keys would be an unauthenticated remote front door, which ADR-0008
// forbids.
func New(opts Options) (*Server, error) {
	allow, err := loadAllowlist(opts.Keys, opts.KeysFile)
	if err != nil {
		return nil, err
	}
	if len(allow) == 0 {
		return nil, errors.New("remote: refusing to start SSH server with an empty authorized-keys allowlist (ADR-0008)")
	}

	listen := opts.Listen
	if listen == "" {
		listen = DefaultListen
	}
	hostKey := opts.HostKeyPath
	if hostKey == "" {
		hostKey = DefaultHostKeyPath()
	}
	// Ensure the host-key directory exists at 0700 before keygen writes into it
	// (keygen also enforces this, but creating it here keeps perms explicit).
	if dir := filepath.Dir(hostKey); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("remote: host key dir: %w", err)
		}
	}

	s := &Server{addr: listen}

	// Public-key auth: only allowlisted keys may connect, and the matched key's
	// read-only flag is stashed on the session context for the TUI to honour.
	authHandler := func(ctx ssh.Context, key ssh.PublicKey) bool {
		ro, ok := allow.authorize(key)
		if !ok {
			return false
		}
		ctx.SetValue(roContextKey, ro)
		return true
	}

	// Each session hosts the SAME TUI Model as a local client of the socket,
	// with a per-session renderer so colorprofile degrades to the remote
	// client's actual terminal (ADR-0004/0008). A read-only key opens attaches
	// as protocol.AttachRO via tui.Options.ReadOnly.
	//
	// This mirrors wish's bubbletea middleware (window-resize bridge + quit on
	// session end) but adds one CRITICAL step it lacks: after Program.Run
	// returns it calls Model.Close(). Bubble Tea returns on QuitMsg without
	// delivering it to Update, so the Model cannot clean up from inside its own
	// loop; without this call every remote session would leak its two daemon
	// socket connections and its read-loop goroutine for the life of the
	// (long-lived) daemon. Close runs strictly after Run so it never races the
	// Update loop's access to the connection fields.
	teaMiddleware := func(next ssh.Handler) ssh.Handler {
		return func(sess ssh.Session) {
			ro, _ := sess.Context().Value(roContextKey).(bool)
			renderer := bm.MakeRenderer(sess)
			m := tui.New(tui.Options{
				Socket:     opts.Socket,
				ConfigPath: opts.ConfigPath,
				Version:    opts.Version,
				Renderer:   renderer,
				ReadOnly:   ro,
			})
			popts := append([]tea.ProgramOption{
				tea.WithAltScreen(), tea.WithMouseCellMotion(),
			}, bm.MakeOptions(sess)...)
			p := tea.NewProgram(m, popts...)

			_, winCh, _ := sess.Pty()
			ctx, cancel := context.WithCancel(sess.Context())
			go func() {
				for {
					select {
					case <-ctx.Done():
						p.Quit()
						return
					case w := <-winCh:
						p.Send(tea.WindowSizeMsg{Width: w.Width, Height: w.Height})
					}
				}
			}()

			_, _ = p.Run()
			p.Kill()
			cancel()
			m.Close()
			next(sess)
		}
	}

	srv, err := wish.NewServer(
		wish.WithAddress(listen),
		wish.WithHostKeyPath(hostKey),
		wish.WithPublicKeyAuth(authHandler),
		wish.WithMiddleware(
			// Innermost first: host the TUI program, then require an active PTY.
			teaMiddleware,
			activeterm.Middleware(),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("remote: build SSH server: %w", err)
	}
	s.ssh = srv
	return s, nil
}

// Addr returns the configured bind address.
func (s *Server) Addr() string { return s.addr }

// Serve binds the listener and serves until Shutdown. It blocks; run it in a
// goroutine. A clean shutdown returns ssh.ErrServerClosed, which Serve maps to
// nil.
func (s *Server) Serve() error {
	err := s.ssh.ListenAndServe()
	if errors.Is(err, ssh.ErrServerClosed) {
		return nil
	}
	return err
}

// ServeListener serves on an already-bound listener (used by tests to bind an
// ephemeral port). It blocks until Shutdown.
func (s *Server) ServeListener(ln net.Listener) error {
	err := s.ssh.Serve(ln)
	if errors.Is(err, ssh.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server, refusing new connections and waiting
// for ctx.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.ssh.Shutdown(ctx)
}
