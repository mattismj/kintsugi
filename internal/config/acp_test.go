package config

import (
	"testing"

	"github.com/BurntSushi/toml"
)

// The ACP section must be inert when present and empty: the adapter's zero value
// is the recommended posture, so a workspace that writes `[acp]` with everything
// commented out must get exactly the behaviour it got before the section existed.
func TestACPZeroValueIsTheRecommendedPosture(t *testing.T) {
	f := Defaults()
	if f.ACP.Off {
		t.Fatal("the adapter defaults on")
	}
	if f.ACP.Timeout != "" || f.ACP.PromptTimeout != "" || f.ACP.CancelGrace != "" {
		t.Fatalf("defaults must leave durations to the package: %+v", f.ACP)
	}

	var empty File
	if _, err := toml.Decode("[acp]\n", &empty); err != nil {
		t.Fatal(err)
	}
	if empty.ACP != (ACP{}) {
		t.Fatalf("an empty [acp] section is not zero: %+v", empty.ACP)
	}
}

func TestACPDecodes(t *testing.T) {
	var f File
	src := `[acp]
off = true
timeout = "45s"
prompt_timeout = "20m"
cancel_grace = "2s"
`
	if _, err := toml.Decode(src, &f); err != nil {
		t.Fatal(err)
	}
	if !f.ACP.Off {
		t.Fatal("off ignored")
	}
	if f.ACP.Timeout != "45s" || f.ACP.PromptTimeout != "20m" || f.ACP.CancelGrace != "2s" {
		t.Fatalf("durations not decoded: %+v", f.ACP)
	}
}
