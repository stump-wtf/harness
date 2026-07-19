package tui

// Governing: SPEC-0001 REQ "Confirmation Guards" — stop/restart/delete present a
// confirm dialog (destructive-action guard), skippable via a --yes-style
// setting. This is the pure gate the model consults before signaling anything
// (scenario "Accidental stop": pressing x on a running harness must intercept
// before anything is signaled).

// Action names a guardable dashboard action.
type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
	ActionDelete  Action = "delete"
)

// destructive is the set of actions that require a confirm dialog. Start is not
// destructive; stop/restart/delete are.
var destructive = map[Action]bool{
	ActionStop:    true,
	ActionRestart: true,
	ActionDelete:  true,
}

// needsConfirm reports whether an action must show a confirm dialog before it is
// carried out. skipConfirm is the --yes-style setting that bypasses the guard.
// A non-destructive action (start) never needs confirmation.
func needsConfirm(a Action, skipConfirm bool) bool {
	if skipConfirm {
		return false
	}
	return destructive[a]
}

// confirmPrompt is the human question shown in the dialog for an action+target.
func confirmPrompt(a Action, target string) string {
	switch a {
	case ActionStop:
		return "Stop " + target + "?"
	case ActionRestart:
		return "Restart " + target + "?"
	case ActionDelete:
		return "Delete " + target + " from harnessd.toml?"
	default:
		return string(a) + " " + target + "?"
	}
}
