// Package webhook is the opt-in HTTP listener that turns authenticated
// deliveries into trigger firings — the second network front door (ADR-0004
// applied to machines).
//
// It is an http.Server of its own, separate from the Wish SSH server and from
// the metrics listener: their exposures are opposite (metrics stays on
// loopback for a scraper; a webhook listener is routinely published through a
// reverse proxy), and one server per surface keeps each bind decision
// independent and keeps a webhook bug from reaching /metrics.
//
// It serves exactly two routes, `POST /hooks/<name>` and `GET /healthz`, and
// runs every delivery through the pipeline design.md orders — cheap checks
// first, the budget spent last:
//
//  1. method
//  2. route
//  3. concurrency slot
//  4. bounded body
//  5. verify
//  6. events
//  7. de-duplication
//  8. rate limit
//  9. fire
//
// Steps 7 and 8 are pass-through seams (#460). It deliberately does not use
// http.ServeMux: a mux cleans paths and answers an unclean one with a 301,
// and REQ "Webhook Routes" forbids the listener to redirect at all.
//
// Governing: ADR-0021, ADR-0004, ADR-0008; SPEC-0014 REQ "Webhook Listener",
// REQ "Webhook Routes", REQ "Webhook Verification", REQ "Webhook Filtering",
// REQ "Webhook Responses", REQ "Concurrency Safety", Security Requirements;
// design.md "Webhook pipeline order", "A separate listener from metrics and
// SSH", "Webhook delivery".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 webhook listener (#458).
package webhook

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/source"
)

// The slow-client bounds REQ "Webhook Listener" fixes.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 30 * time.Second
	DefaultIdleTimeout       = 60 * time.Second
	// DefaultMaxHeaderBytes bounds request headers.
	DefaultMaxHeaderBytes = 64 << 10
	// DefaultMaxConcurrent is how many deliveries are handled at once; the
	// excess is answered 503.
	DefaultMaxConcurrent = 64
	// ShutdownGrace is how long in-flight requests get once shutdown begins.
	ShutdownGrace = 5 * time.Second
)

// hooksPrefix is the path every delivery route lives under.
const hooksPrefix = "/hooks/"

// Firer is where a verified, filtered delivery goes: the trigger source
// manager, which fans it out to every bound harness and reports one decision
// per harness. *source.Manager implements it.
type Firer interface {
	Fire(ev *trigger.Envelope) []source.Decision
}

// Settings is what binding the listener depends on. A reload that changes any
// of it needs a daemon restart; see Reload.
type Settings struct {
	// Addr is the host:port to bind.
	Addr string
	// TLSCertFile and TLSKeyFile, both set, make the listener HTTPS-only.
	TLSCertFile string
	TLSKeyFile  string
}

// TLS reports whether these settings serve HTTPS.
func (s Settings) TLS() bool { return s.TLSCertFile != "" && s.TLSKeyFile != "" }

// SettingsFrom reads the listener settings from a config, with addrOverride
// (from --webhook-listen or HARNESS_WEBHOOK_LISTEN) winning over
// `[server] webhook_listen` when it is set (SPEC-0010 REQ "Precedence Order").
func SettingsFrom(sc core.ServerConfig, addrOverride string) Settings {
	addr := strings.TrimSpace(sc.WebhookListen)
	if o := strings.TrimSpace(addrOverride); o != "" {
		addr = o
	}
	return Settings{Addr: addr, TLSCertFile: sc.WebhookTLSCertFile, TLSKeyFile: sc.WebhookTLSKeyFile}
}

// Options configure a Server. The zero value of every duration and limit is
// the REQ "Webhook Listener" default; a test shortens them.
type Options struct {
	Settings
	// Firer receives firings. Required.
	Firer Firer
	// Log defaults to the package logger.
	Log *log.Logger
	// Now is the clock a delivery's received_at (and a daemon-made event
	// ID) is stamped from. Defaults to time.Now. It is the one place this
	// package reads the time, so the daemon can hand it the same clock the
	// source manager and scheduler use.
	Now func() time.Time

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	MaxConcurrent     int

	// newVerifier is the scheme registry; nil means NewVerifier. Unexported:
	// only this package's tests substitute it, to observe whether a verifier
	// ran at all.
	newVerifier NewVerifierFunc
}

// Server is the webhook listener.
type Server struct {
	settings    Settings
	firer       Firer
	log         *log.Logger
	now         func() time.Time
	newVerifier NewVerifierFunc

	// routes is the current route table, swapped whole on reload.
	routes atomic.Pointer[table]
	// slots is the concurrency semaphore: a delivery holds one from route
	// lookup until it has answered.
	slots chan struct{}
	// inFlight counts held slots, for tests and for #480's gauge.
	inFlight atomic.Int64

	hs *http.Server

	mu sync.Mutex
	ln net.Listener
}

// New builds a Server over cfg's routes. It binds nothing until Listen.
func New(opts Options, cfg *core.Config) *Server {
	logger := opts.Log
	if logger == nil {
		logger = log.Default()
	}
	nv := opts.newVerifier
	if nv == nil {
		nv = NewVerifier
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Server{
		settings:    opts.Settings,
		firer:       opts.Firer,
		log:         logger,
		now:         now,
		newVerifier: nv,
		slots:       make(chan struct{}, orDefault(opts.MaxConcurrent, DefaultMaxConcurrent)),
	}
	s.routes.Store(buildTable(cfg, nv, logger))
	s.hs = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: orDefault(opts.ReadHeaderTimeout, DefaultReadHeaderTimeout),
		ReadTimeout:       orDefault(opts.ReadTimeout, DefaultReadTimeout),
		WriteTimeout:      orDefault(opts.WriteTimeout, DefaultWriteTimeout),
		IdleTimeout:       orDefault(opts.IdleTimeout, DefaultIdleTimeout),
		MaxHeaderBytes:    orDefault(opts.MaxHeaderBytes, DefaultMaxHeaderBytes),
		// net/http's own error log (TLS handshake failures, a panicking
		// handler) goes to the daemon log rather than stderr.
		ErrorLog: logger.StandardLog(log.StandardLogOptions{ForceLevel: log.WarnLevel}),
	}
	return s
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// Listen binds the listener synchronously, so a nil error means the port is
// held and a delivery will reach it. With both TLS files set it loads the key
// pair here — a bad pair fails the bind, never a first delivery — and serves
// HTTPS only.
func (s *Server) Listen() error {
	if s.settings.Addr == "" {
		return errors.New("webhook: no listen address")
	}
	if s.settings.TLS() {
		pair, err := tls.LoadX509KeyPair(s.settings.TLSCertFile, s.settings.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("webhook: load TLS key pair: %w", err)
		}
		s.hs.TLSConfig = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	}
	ln, err := net.Listen("tcp", s.settings.Addr)
	if err != nil {
		return fmt.Errorf("webhook: bind %s: %w", s.settings.Addr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	return nil
}

// Serve serves on the bound listener until Shutdown. It returns nil after a
// clean shutdown.
func (s *Server) Serve() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return errors.New("webhook: Serve before Listen")
	}
	var err error
	if s.settings.TLS() {
		// Empty file names: the pair loaded in Listen is in TLSConfig.
		err = s.hs.ServeTLS(ln, "", "")
	} else {
		err = s.hs.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Addr is the bound address — the real port when Settings.Addr asked for :0 —
// or Settings.Addr before Listen.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return s.settings.Addr
}

// Settings returns what the listener was started with.
func (s *Server) Settings() Settings { return s.settings }

// InFlight is how many deliveries hold a concurrency slot right now.
func (s *Server) InFlight() int { return int(s.inFlight.Load()) }

// Reload swaps in the route table cfg describes, and reports whether want —
// the listener settings the reloaded config asks for — differs from what the
// listener is bound with.
//
// The table always swaps; the bind never moves. Rebinding under a live
// daemon would mean a window with no listener, or two, and a TLS change that
// failed to load halfway through a reload would leave the route unserved. So
// a changed address or TLS pair is logged as needing a restart, and the
// listener keeps serving on the old settings (REQ "Webhook Listener").
// Governing: SPEC-0014 REQ "Webhook Listener", REQ "Source Reconciliation On
// Reload".
func (s *Server) Reload(cfg *core.Config, want Settings) (restartRequired bool) {
	s.routes.Store(buildTable(cfg, s.newVerifier, s.log))
	if want == s.settings {
		return false
	}
	s.log.Warn("webhook listener settings changed; restart the daemon to apply them (still serving on the old settings)",
		"addr", s.settings.Addr, "want_addr", want.Addr,
		"tls", s.settings.TLS(), "want_tls", want.TLS())
	return true
}

// Shutdown stops accepting connections and gives in-flight requests until ctx
// ends to finish, then closes whatever is left.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.hs.Shutdown(ctx)
	if err != nil {
		// The grace ran out: cut the stragglers rather than hang the
		// daemon's exit on a sender that will not finish.
		_ = s.hs.Close()
	}
	return err
}

// ServeHTTP runs one request through the pipeline.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tlsOn := r.TLS != nil
	path := r.URL.Path

	if path == "/healthz" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, tlsOn, http.StatusMethodNotAllowed, errMethodNotAllowed)
			return
		}
		// A constant: no sources, no harnesses, no version.
		writeResponse(w, tlsOn, http.StatusOK, "text/plain; charset=utf-8", []byte("ok"))
		return
	}

	name, ok := hookName(path)
	if !ok {
		writeError(w, tlsOn, http.StatusNotFound, errNotFound)
		return
	}

	// 1. Method. Before the route lookup, so a GET cannot tell a served name
	// from an unserved one either.
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, tlsOn, http.StatusMethodNotAllowed, errMethodNotAllowed)
		return
	}

	// 2. Route. The table is loaded ONCE: everything below uses this route,
	// even if a reload swaps the table while the body is still arriving.
	rt := s.routes.Load().lookup(name)
	if rt == nil {
		writeError(w, tlsOn, http.StatusNotFound, errNotFound)
		return
	}

	// 3. Concurrency slot. Never waited for: a queue here would let a flood
	// of slow senders hold every request behind them.
	select {
	case s.slots <- struct{}{}:
		s.inFlight.Add(1)
		defer func() {
			s.inFlight.Add(-1)
			<-s.slots
		}()
	default:
		s.log.Warn("webhook delivery refused: listener at its concurrency limit", "source", rt.ref, "peer", r.RemoteAddr, "limit", cap(s.slots))
		writeError(w, tlsOn, http.StatusServiceUnavailable, errUnavailable)
		return
	}

	// 4. Bounded body, before verification: every scheme signs over the
	// whole body, so the cap is what bounds the verifier's work.
	if r.ContentLength > rt.src.MaxBody {
		s.log.Warn("webhook delivery refused: body over max_body", "source", rt.ref, "peer", r.RemoteAddr, "max_body", rt.src.MaxBody)
		writeError(w, tlsOn, http.StatusRequestEntityTooLarge, errPayloadTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rt.src.MaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.log.Warn("webhook delivery refused: body over max_body", "source", rt.ref, "peer", r.RemoteAddr, "max_body", rt.src.MaxBody)
			writeError(w, tlsOn, http.StatusRequestEntityTooLarge, errPayloadTooLarge)
			return
		}
		s.log.Warn("webhook delivery dropped: body read failed", "source", rt.ref, "peer", r.RemoteAddr, "err", err.Error())
		writeError(w, tlsOn, http.StatusBadRequest, errBadRequest)
		return
	}

	// 5. Verify. The error names the failed rule, never the value presented,
	// so it is safe to log; the response is the same bytes for every cause.
	if err := rt.verifier.Verify(r.Header, body); err != nil {
		s.log.Warn("webhook delivery refused: verification failed", "source", rt.ref, "peer", r.RemoteAddr, "reason", err.Error())
		writeError(w, tlsOn, http.StatusUnauthorized, errUnauthorized)
		return
	}

	ev := newEnvelope(rt, r, body, s.now().UTC())

	// 6. Events.
	if !rt.eventAllowed(ev.Webhook.Event) {
		s.log.Debug("webhook delivery ignored: event not in the allowlist", "source", rt.ref)
		writeResponse(w, tlsOn, http.StatusAccepted, "application/json", ignoredBody(rt.src.Name))
		return
	}
	// 7. De-duplication and 8. rate limit: pass-through seams. #460 adds
	// them here, after `events` so a filtered delivery costs no budget, and
	// before the firing so a rate-limited one is never recorded as seen.

	// 9. Fire. StartRun returns once each harness's actor loop has decided,
	// so this answers without waiting for any run to finish.
	decisions := s.firer.Fire(ev)
	writeResponse(w, tlsOn, http.StatusAccepted, "application/json", firedBody(rt.src.Name, ev.EventID, firingsFrom(decisions)))
}

// hookName extracts <name> from "/hooks/<name>", reporting false for any path
// that is not exactly that shape: "/hooks/", "/hooks/a/b", "/hooks//a" and a
// name outside the source grammar are all plain 404s.
func hookName(path string) (string, bool) {
	name, ok := strings.CutPrefix(path, hooksPrefix)
	if !ok || !core.SourceNameRe.MatchString(name) {
		return "", false
	}
	return name, true
}

// eventAllowed applies the `events` allowlist. With no list every delivery
// passes; with one, a delivery with no event name is not in it.
// Governing: SPEC-0014 REQ "Webhook Filtering" (item 1).
func (rt *route) eventAllowed(event string) bool {
	if len(rt.src.Events) == 0 {
		return true
	}
	if event == "" {
		return false
	}
	for _, e := range rt.src.Events {
		if e == event {
			return true
		}
	}
	return false
}

// newEnvelope builds the event a verified delivery becomes. Only the headers
// trigger.AllowedHeaders passes are carried — never Authorization, a cookie or
// the signature — and the body is stored verbatim, uninterpreted.
// Governing: SPEC-0014 REQ "Event Delivery To The Run", REQ "Run Record
// Fields".
func newEnvelope(rt *route, r *http.Request, body []byte, at time.Time) *trigger.Envelope {
	wh := &trigger.WebhookEvent{
		ContentType: r.Header.Get("Content-Type"),
		Headers:     trigger.AllowedHeaders(rt.src, r.Header.Get),
	}
	if rt.src.EventHeader != "" {
		wh.Event = r.Header.Get(rt.src.EventHeader)
	}
	if rt.src.DeliveryHeader != "" {
		wh.Delivery = r.Header.Get(rt.src.DeliveryHeader)
	}
	wh.SetBody(wh.ContentType, body)
	id := wh.Delivery
	if !usableEventID(id) {
		id = newEventID(at)
	}
	return &trigger.Envelope{
		Version:    trigger.EnvelopeVersion,
		Kind:       trigger.KindWebhook,
		Source:     rt.ref,
		EventID:    id,
		ReceivedAt: at,
		Webhook:    wh,
	}
}

// maxEventIDLen bounds a sender-supplied delivery ID used as the run record's
// event_id. Real ones are UUIDs or short hex; anything longer is not an ID.
const maxEventIDLen = 128

// usableEventID reports whether a sender's delivery ID can stand as the run
// record's event_id: non-empty, bounded, and printable ASCII with no spaces.
// The ID is sender-controlled and lands in state.json and `harness runs`; a
// value that fails is not refused — the delivery was authentic — but the
// record gets a daemon-made ID instead, and the raw value stays in the event
// file, where untrusted text belongs.
func usableEventID(id string) bool {
	if id == "" || len(id) > maxEventIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// eventIDs numbers daemon-made IDs within one timestamp.
var eventIDs atomic.Int64

// newEventID makes an ID for a delivery that carried no usable one, so two
// such firings stay distinguishable in a run history.
func newEventID(at time.Time) string {
	return fmt.Sprintf("wh-%s-%d", at.Format("20060102T150405.000"), eventIDs.Add(1))
}
