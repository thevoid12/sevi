// Package event is the user-facing channel: what an adapter may show a person.
// Logs (zap) are for the developer and carry everything; events carry only
// safe, plain-language text. Core emits events and never writes to a chat.
package event

import "errors"

type Kind string

const (
	Progress Kind = "progress" // a step is under way ("Generating audio 3/12")
	Notice   Kind = "notice"   // information, not an error
	Confirm  Kind = "confirm"  // a question that needs a yes/no before continuing
	Error    Kind = "error"    // the job failed; Message is safe to show
	Cost     Kind = "cost"     // final cost of the job is known
	Done     Kind = "done"     // the job finished
)

type Event struct {
	Seq     int      `json:"seq"`
	Kind    Kind     `json:"kind"`
	Message string   `json:"message"`
	Step    int      `json:"step,omitempty"`
	Total   int      `json:"total,omitempty"`
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// Ref is the request id of a failure, so the matching log lines can be found.
	Ref string `json:"ref,omitempty"`
}

// UserError is a failure with a safe message for the user and a Cause for the
// logs. Cause (raw provider text, paths, stack) must never reach an adapter.
type UserError struct {
	Message string
	Cause   error
}

func (e *UserError) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return e.Message + ": " + e.Cause.Error()
}

func (e *UserError) Unwrap() error { return e.Cause }

// Wrap builds a UserError.
func Wrap(message string, cause error) *UserError { return &UserError{Message: message, Cause: cause} }

// SafeMessage returns the user-safe text for err: the UserError message if
// there is one, otherwise a generic message (never the raw error text).
func SafeMessage(err error) string {
	var ue *UserError
	if errors.As(err, &ue) {
		return ue.Message
	}
	return "Something went wrong on my side."
}
