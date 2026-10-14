// Package apperr defines error types whose message is safe to show to API clients.
// Any error that is not a *Validation must be treated as internal: logged, never echoed.
package apperr

import (
	"errors"
	"fmt"
)

// Validation is a client-caused error (bad input). Its message is returned verbatim.
type Validation struct {
	Msg string
	Err error // optional sentinel for errors.Is
}

func (v *Validation) Error() string { return v.Msg }
func (v *Validation) Unwrap() error { return v.Err }

// Invalid builds a validation error with a client-safe message.
func Invalid(format string, args ...any) error {
	return &Validation{Msg: fmt.Sprintf(format, args...)}
}

// Wrap marks sentinel as a validation error with a client-safe message.
func Wrap(sentinel error, msg string) error {
	return &Validation{Msg: msg, Err: sentinel}
}

// AsValidation reports whether err (or something it wraps) is a validation error.
func AsValidation(err error) (*Validation, bool) {
	var v *Validation
	if errors.As(err, &v) {
		return v, true
	}
	return nil, false
}
