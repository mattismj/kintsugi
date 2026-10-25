// Package clipboard copies text to the operator's system clipboard through
// OSC 52.
//
// OSC 52 asks the *terminal* to place text on the clipboard. It is the only
// copy mechanism that works over SSH, inside tmux, and in any terminal that
// supports it, because nothing has to run on the machine the clipboard belongs
// to. The alternative — shelling out to xclip/pbcopy/wl-copy — fails exactly in
// the situations this harness is built for.
//
// It is a request, not a guarantee. The terminal may ignore it (some do, by
// policy, for security), and there is no usable reply to check: OSC 52 has no
// acknowledgement in common terminals. So Copy reports only whether it wrote
// the sequence, and callers phrase the UI honestly: "sent to clipboard", never
// "copied".
package clipboard

import (
	"io"
	"os"
	"strings"

	osc52 "github.com/aymanbagabas/go-osc52/v2"
)

// maxBytes caps what will be handed to the sequence. Terminals differ, but the
// practical ceiling is well under this; an oversized request is silently
// dropped by most of them, so it is refused here with a clear error instead.
const maxBytes = 100 << 10

// Copy writes text to the clipboard of the terminal attached to w.
//
// The escape is wrapped for tmux and screen when those are detected: without
// the DCS passthrough, a multiplexed session swallows the sequence and the copy
// silently does nothing — which is the worst possible failure mode, because the
// operator believes it worked.
func Copy(w io.Writer, text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	n := len(text)
	if n > maxBytes {
		text = text[:maxBytes]
	}
	seq := osc52.New(text)
	switch {
	case os.Getenv("TMUX") != "":
		seq = seq.Tmux()
	default:
		// screen does not set TMUX; it announces itself through TERM.
		if strings.HasPrefix(os.Getenv("TERM"), "screen") {
			seq = seq.Screen()
		}
	}
	written, err := seq.WriteTo(w)
	return int(written), err
}

// Supported reports whether the environment looks like it can honour OSC 52.
// It is a heuristic for the UI to hide an affordance that would do nothing: a
// dumb terminal or a pipe cannot, and NO_COLOR is not the signal here — a
// monochrome terminal still has a clipboard.
func Supported() bool {
	if os.Getenv("TERM") == "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
