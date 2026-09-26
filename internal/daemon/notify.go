package daemon

// Notify Control Surface
//
// daemon_info reports the [notify] hook the daemon is running with and its
// last delivery; notify_test runs the hook once, now, so an operator can prove
// the whole path — the daemon's environment, the script, whatever it talks to
// — before the first real failure depends on it. The test answers from its own
// goroutine: a hook may take up to its timeout (as long as five minutes), and
// the connection's read loop must keep answering the heartbeat meanwhile or
// the liveness reaper would drop the very client waiting for the answer.
//
// Governing: SPEC-0003 REQ "Operator Notification"; SPEC-0002 REQ "Control
// Operations"; issue #725.

import (
	"context"
	"time"

	"github.com/stump-wtf/harness/internal/notify"
	"github.com/stump-wtf/harness/internal/protocol"
)

// Notifier is the notify dispatcher surface the daemon serves.
// *notify.Dispatcher satisfies it.
type Notifier interface {
	Status() notify.Status
	Test(ctx context.Context) notify.Delivery
}

// notifyInfo is daemon_info's Notify: nil when no hook is configured.
func (s *Server) notifyInfo() *protocol.NotifyInfo {
	if s.notifier == nil {
		return nil
	}
	st := s.notifier.Status()
	if !st.Config.Enabled() {
		return nil
	}
	info := &protocol.NotifyInfo{
		Command:  st.Config.Command[0],
		Events:   st.Config.Events,
		Timeout:  st.Config.Timeout.String(),
		Cooldown: st.Config.Cooldown.String(),
	}
	if st.Last != nil {
		d := deliveryWire(*st.Last)
		info.Last = &d
	}
	return info
}

// opNotifyTest runs the hook once and answers with the outcome.
func (c *conn) opNotifyTest(req protocol.ControlReq) {
	if c.srv.notifier == nil {
		c.respond(req, protocol.NotifyDelivery{Event: "test", Result: notify.ResultError, Error: "notify is not configured: add a [notify] table to harness.toml"})
		return
	}
	go func() {
		c.respond(req, deliveryWire(c.srv.notifier.Test(context.Background())))
	}()
}

func deliveryWire(d notify.Delivery) protocol.NotifyDelivery {
	out := protocol.NotifyDelivery{
		Event:      d.Event,
		Harness:    d.Harness,
		Result:     d.Result,
		Error:      d.Error,
		DurationMs: d.Duration.Milliseconds(),
	}
	if !d.At.IsZero() {
		out.At = d.At.Format(time.RFC3339)
	}
	return out
}
