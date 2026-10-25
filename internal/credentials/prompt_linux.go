//go:build linux

package credentials

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// ReadHidden reads one line from the terminal with echo disabled: the key is
// never displayed, never passed through a shell and never written anywhere.
// Canonical mode is kept, so line editing still works; only ECHO is cleared.
// The prompt goes to stderr so stdout stays pipeable.
//
// A non-terminal stdin (pipe, CI, /dev/null) is refused rather than read:
// silent echo suppression is impossible there, and the operator may be piping
// a key by mistake. Scripts should pass --key-stdin instead.
func ReadHidden(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())

	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", errors.New("not a terminal; use --key-stdin")
	}

	noEcho := *old
	noEcho.Lflag &^= unix.ECHO
	noEcho.Lflag |= unix.ICANON | unix.ISIG

	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &noEcho); err != nil {
		return "", fmt.Errorf("cannot disable terminal echo: %w", err)
	}
	// Restore before anything can panic, and always close the operator's line.
	defer func() {
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, old)
		_, _ = os.Stderr.WriteString("\n")
	}()

	if prompt != "" {
		if _, err := os.Stderr.WriteString(prompt); err != nil {
			return "", fmt.Errorf("cannot write prompt: %w", err)
		}
	}

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		// Do not wrap the raw error text: it can quote input bytes.
		return "", errors.New("cannot read key from terminal")
	}
	if err != nil && line == "" {
		return "", errors.New("no key entered")
	}
	return strings.TrimRight(line, "\r\n"), nil
}
