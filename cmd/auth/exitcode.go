package auth

import (
	"errors"

	"github.com/spf13/cobra"
)

// Exit codes of the process. 0 is success; 3 and 4 are only returned by
// the password and confirmation paths of "auth add" / "auth refresh".
const (
	// ExitError is any failure without a more specific code.
	ExitError = 1
	// ExitUsage is a wrong argument, flag or flag combination, or an
	// unusable password on standard input.
	ExitUsage = 2
	// ExitBadCredentials is a password login the Tuya cloud rejected as
	// wrong account or password. Tuya sends the same answer for OEM accounts
	// that only support QR login, so the password is not always wrong.
	ExitBadCredentials = 3
	// ExitNotConfirmed is an existing account that "auth add" was not
	// allowed to re-authenticate because --yes is missing and standard
	// input is used for the password.
	ExitNotConfirmed = 4
)

// CodedError is an error that carries the process exit code.
type CodedError struct {
	Code int
	Err  error
}

// Error returns the message of the wrapped error.
func (e *CodedError) Error() string { return e.Err.Error() }

// Unwrap returns the wrapped error.
func (e *CodedError) Unwrap() error { return e.Err }

// ExitCode returns the process exit code for err: 0 for nil, the code of
// the first CodedError in the chain, otherwise ExitError.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ExitError
}

func usageError(err error) error {
	return &CodedError{Code: ExitUsage, Err: err}
}

// usageArgs makes the exit code of a wrong argument count ExitUsage.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return usageError(err)
		}
		return nil
	}
}

// usageFlagError makes the exit code of an unknown or malformed flag
// ExitUsage.
func usageFlagError(cmd *cobra.Command, err error) error {
	return usageError(err)
}

// addNonInteractiveFlags adds --yes and --password-stdin to cmd.
func addNonInteractiveFlags(cmd *cobra.Command) {
	cmd.Flags().BoolP("yes", "y", false,
		"Answer the \"already exists, re-authenticate?\" question with yes without asking")
	cmd.Flags().Bool("password-stdin", false,
		"Read the password as one line from standard input (requires --password)")
}
