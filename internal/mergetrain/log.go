package mergetrain

// Logging
//
// Every transition is one structured line whose message is "mergetrain
// <event>" (SPEC-0025 REQ-14). mergetrain takes this interface rather than a
// concrete logger: charmbracelet/log's *Logger satisfies it, so the daemon
// passes its own, and tests pass a recorder.

// Logger is the subset of charmbracelet/log the train uses.
type Logger interface {
	Info(msg interface{}, keyvals ...interface{})
	Warn(msg interface{}, keyvals ...interface{})
	Error(msg interface{}, keyvals ...interface{})
}

type nopLogger struct{}

func (nopLogger) Info(interface{}, ...interface{})  {}
func (nopLogger) Warn(interface{}, ...interface{})  {}
func (nopLogger) Error(interface{}, ...interface{}) {}

// event is the message for a transition.
func event(name string) string { return "mergetrain " + name }
