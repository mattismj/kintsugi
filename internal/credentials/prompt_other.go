//go:build !linux

package credentials

import "errors"

// ReadHidden is unavailable off Linux: there is no portable, dependency-free
// way to clear terminal echo here, and echoing a key in the clear is worse
// than refusing. Callers must use --key-stdin.
func ReadHidden(prompt string) (string, error) {
	return "", errors.New("hidden key input is not supported on this platform; use --key-stdin")
}
