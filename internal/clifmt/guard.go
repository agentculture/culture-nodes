package clifmt

import "errors"

// Guard runs fn and recovers any panic, converting it into a CliError in
// the ExitEnvError bucket so no stack trace ever reaches the user. Any
// non-nil error fn returns is normalised the same way, *unless* it already
// is (or wraps) a *CliError, in which case that CliError passes through
// with its own Code intact — a deliberately raised user error (code 1)
// must not be reclassified as an environment error (code 2).
//
// Guard returns nil on success.
func Guard(fn func() error) (result *CliError) {
	defer func() {
		if r := recover(); r != nil {
			result = NewEnvError(r)
		}
	}()

	err := fn()
	if err == nil {
		return nil
	}

	var cliErr *CliError
	if errors.As(err, &cliErr) {
		return cliErr
	}
	return NewEnvError(err)
}
