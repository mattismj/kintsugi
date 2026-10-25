package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kintsugi/internal/harness"
)

// testApp builds a real App on throwaway config and workspace directories, so
// Run exercises the same path an operator's `kintsugi doctor` does — including the
// embedded persona-pack fallback and the default model's provider resolution.
func testApp(t *testing.T, defaultModel string) *harness.App {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	app, err := harness.New(t.TempDir())
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	if defaultModel != "" {
		app.Cfg.Core.DefaultModel = defaultModel
	}
	return app
}

// writeCredentialStore writes the credential store in the shape the credentials
// package documents at $XDG_CONFIG_HOME/kintsugi/credentials.json.
func writeCredentialStore(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kintsugi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func findCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %d checks", name, len(checks))
	return Check{}
}

// TestProviderKeyCheckReportsEnvStoredAndMissing covers the three states the
// provider-key check distinguishes: an exported env var, a key stored by
// `kintsugi connect`, and nothing at all. The detail strings are pinned because the
// missing state is the only place doctor tells the operator what to run.
func TestProviderKeyCheckReportsEnvStoredAndMissing(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		app := testApp(t, "deepseek/deepseek-chat")
		t.Setenv("DEEPSEEK_API_KEY", "sk-env")
		c := findCheck(t, Run(app, false), "provider-key")
		if c.State != StatePass || c.Detail != "DEEPSEEK_API_KEY set" {
			t.Fatalf("provider-key = %+v, want pass with %q", c, "DEEPSEEK_API_KEY set")
		}
	})
	t.Run("stored", func(t *testing.T) {
		app := testApp(t, "deepseek/deepseek-chat")
		t.Setenv("DEEPSEEK_API_KEY", "")
		writeCredentialStore(t, `{"version":1,"keys":{"deepseek":"sk-stored"}}`)
		c := findCheck(t, Run(app, false), "provider-key")
		if c.State != StatePass || c.Detail != "stored locally (kintsugi connect deepseek)" {
			t.Fatalf("provider-key = %+v, want pass with the stored detail", c)
		}
	})
	t.Run("missing", func(t *testing.T) {
		app := testApp(t, "deepseek/deepseek-chat")
		t.Setenv("DEEPSEEK_API_KEY", "")
		c := findCheck(t, Run(app, false), "provider-key")
		if c.State != StateFail {
			t.Fatalf("provider-key = %+v, want fail", c)
		}
		for _, want := range []string{"missing: run kintsugi connect deepseek", "DEEPSEEK_API_KEY"} {
			if !strings.Contains(c.Detail, want) {
				t.Fatalf("detail %q missing %q", c.Detail, want)
			}
		}
	})
}

// TestProviderKeyCheckNamesTheResolvedProvider proves the hint names the
// provider the default model resolves to, not a hard-coded one — the Alibaba plan
// entries and their keys are the case where that difference matters.
func TestProviderKeyCheckNamesTheResolvedProvider(t *testing.T) {
	app := testApp(t, "alibaba-token-plan/qwen3-max")
	t.Setenv("ALIBABA_TOKEN_PLAN_API_KEY", "")
	c := findCheck(t, Run(app, false), "provider-key")
	if c.State != StateFail {
		t.Fatalf("provider-key = %+v, want fail", c)
	}
	if !strings.Contains(c.Detail, "kintsugi connect alibaba-token-plan") ||
		!strings.Contains(c.Detail, "ALIBABA_TOKEN_PLAN_API_KEY") {
		t.Fatalf("detail = %q, want the token-plan provider and env", c.Detail)
	}
}

// TestProviderKeyCheckNotRequiredForLocalEndpoint keeps the loopback providers
// out of the credential story: nothing to connect, so the check passes with its
// own detail instead of demanding a key that does not exist.
func TestProviderKeyCheckNotRequiredForLocalEndpoint(t *testing.T) {
	app := testApp(t, "ollama/qwen2.5-coder")
	c := findCheck(t, Run(app, false), "provider-key")
	if c.State != StatePass || c.Detail != "not required (local endpoint)" {
		t.Fatalf("provider-key = %+v, want the local-endpoint detail", c)
	}
}

// TestMissingDefaultModelAndPersonaPackFail checks the two plan-item E3
// blockers: a config with no default model and no persona pack must yield at
// least two fail checks, each naming the missing config key it needs set.
func TestMissingDefaultModelAndPersonaPackFail(t *testing.T) {
	app := testApp(t, "")
	app.Cfg.Core.DefaultModel = ""
	app.Pack = nil
	checks := Run(app, false)

	dm := findCheck(t, checks, "default-model")
	if dm.State != StateFail || !strings.Contains(dm.Detail, "core.default_model") {
		t.Fatalf("default-model = %+v, want fail naming core.default_model", dm)
	}

	pp := findCheck(t, checks, "persona-pack")
	if pp.State != StateFail || !strings.Contains(pp.Detail, "persona_pack.dir") {
		t.Fatalf("persona-pack = %+v, want fail naming persona_pack.dir", pp)
	}

	var failCount int
	for _, c := range checks {
		if c.EffectiveState() == StateFail {
			failCount++
			if c.Detail == "" {
				t.Fatalf("fail check %q has empty detail", c.Name)
			}
		}
	}
	if failCount < 2 {
		t.Fatalf("want at least 2 fail checks, got %d", failCount)
	}
}

// TestTwoFailRowsNamingMissingKeys uses a real minimal App whose default model
// is unresolvable and whose pack dir points at a nonexistent directory, so the
// persona-pack check fails even though testApp would otherwise fall back to the
// embedded pack.
func TestTwoFailRowsNamingMissingKeys(t *testing.T) {
	app := testApp(t, "does-not-exist/no-such-model")
	app.Cfg.PersonaPack.Dir = filepath.Join(t.TempDir(), "not-a-pack")
	app.Pack = nil

	checks := Run(app, false)
	dm := findCheck(t, checks, "default-model")
	if dm.State != StateFail {
		t.Fatalf("default-model = %+v, want fail", dm)
	}
	pp := findCheck(t, checks, "persona-pack")
	if pp.State != StateFail || !strings.Contains(pp.Detail, "persona_pack.dir") {
		t.Fatalf("persona-pack = %+v, want fail naming persona_pack.dir", pp)
	}
}

// TestAgentsMDMissingIsWarnNotFail pins the optional-missing reclassification:
// no AGENTS.md yields a warn, never a fail.
func TestAgentsMDMissingIsWarnNotFail(t *testing.T) {
	app := testApp(t, "ollama/qwen2.5-coder")
	c := findCheck(t, Run(app, false), "agents-md")
	if c.EffectiveState() != StateWarn {
		t.Fatalf("agents-md = %+v, want warn", c)
	}
	if !strings.Contains(c.Detail, "AGENTS.md") {
		t.Fatalf("detail %q must name what is missing", c.Detail)
	}
}

// TestOutputOrderingPutsFailBeforeWarnBeforePass asserts the returned slice is
// sorted by severity: every fail, then every warn, then every pass. This is the
// operator-facing guarantee behind "blockers before niceties".
func TestOutputOrderingPutsFailBeforeWarnBeforePass(t *testing.T) {
	app := testApp(t, "does-not-exist/no-such-model")
	checks := Run(app, false)

	phase := StateFail
	for i, c := range checks {
		got := c.EffectiveState()
		switch phase {
		case StateFail:
			if got == StateWarn {
				phase = StateWarn
			} else if got == StatePass {
				phase = StatePass
			}
		case StateWarn:
			if got == StateFail {
				t.Fatalf("check %d (%s) is fail, but appears after a warn", i, c.Name)
			}
			if got == StatePass {
				phase = StatePass
			}
		case StatePass:
			if got != StatePass {
				t.Fatalf("check %d (%s) is %s, but appears after a pass", i, c.Name, got)
			}
		}
	}
}
