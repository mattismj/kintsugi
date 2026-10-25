// Package setup materializes the shipped persona pack and writes the global
// configuration and AGENTS.md template so a fresh install is editable.
package setup

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"kintsugi/internal/config"
	"kintsugi/pack"
)

type Result struct {
	Root       string
	ConfigPath string
	AgentsPath string
	PackDir    string
	Model      string
	Written    []string
	Skipped    []string
}

// DetectOllama returns the first locally installed Ollama model, or "" when
// the loopback endpoint is not reachable. It never contacts a non-loopback host.
func DetectOllama(timeout time.Duration) string {
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get("http://127.0.0.1:11434/api/tags")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || len(body.Models) == 0 {
		return ""
	}
	return body.Models[0].Name
}

// Materialize copies the embedded agents pack into root/agents. Existing files
// are skipped unless force is set.
func Materialize(root string, force bool) (written, skipped []string, err error) {
	dest := filepath.Join(root, "agents")
	// Never silently write through a symlink: the operator may have linked the
	// repo pack for development. Without --force we skip; with --force we
	// replace the link with a real, independently editable copy.
	if fi, lerr := os.Lstat(dest); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if !force {
			return nil, []string{"skipped " + dest + " (symlink; --force to materialize a real copy)"}, nil
		}
		if err := os.Remove(dest); err != nil {
			return nil, nil, err
		}
	}
	err = fs.WalkDir(pack.FS, "agents", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "agents/")
		if rel == p {
			rel = ""
		}
		out := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := fs.ReadFile(pack.FS, p)
		if err != nil {
			return err
		}
		if _, statErr := os.Stat(out); statErr == nil && !force {
			skipped = append(skipped, out)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, b, 0o644); err != nil {
			return err
		}
		written = append(written, out)
		return nil
	})
	return written, skipped, err
}

// CloudProvider is one open-weights cloud provider the harness can start
// with. The product targets these first: open-weights models over an endpoint
// the operator chooses and pays for, while their prompts, memory and logs stay
// on their own disk. Local runners are supported, but they are a deliberate
// choice — not the default posture.
type CloudProvider struct {
	Name   string // registry key
	KeyEnv string
	Model  string // known model id; empty when the catalogue must be fetched first
	Hint   string // shown when Model is empty
}

// CloudProviders lists the open-weights providers setup prefers, in order.
func CloudProviders() []CloudProvider {
	return []CloudProvider{
		{Name: "deepseek", KeyEnv: "DEEPSEEK_API_KEY", Model: "deepseek/deepseek-chat"},
		{Name: "moonshot", KeyEnv: "MOONSHOT_API_KEY", Model: "moonshot/kimi-k2-turbo-preview"},
		{Name: "zhipu", KeyEnv: "ZHIPU_API_KEY", Model: "zhipu/glm-4.6"},
		{Name: "dashscope", KeyEnv: "DASHSCOPE_API_KEY", Model: "dashscope/qwen3-coder-plus"},
		{Name: "alibaba-coding-plan", KeyEnv: "ALIBABA_CODING_PLAN_API_KEY", Model: "alibaba-coding-plan/qwen3-coder-plus"},
		{Name: "alibaba-token-plan", KeyEnv: "ALIBABA_TOKEN_PLAN_API_KEY", Model: "alibaba-token-plan/deepseek-v4.1-flash"},
		{Name: "openrouter", KeyEnv: "OPENROUTER_API_KEY", Model: "openrouter/qwen/qwen3-coder"},
		{Name: "cline-pass", KeyEnv: "CLINE_PASS_API_KEY", Model: "cline-pass/kimi-k3"},
		{Name: "opencode-go", KeyEnv: "OPENCODE_GO_API_KEY",
			Hint: "run `kintsugi connect opencode-go` then `kintsugi models fetch opencode-go --write` to pick a model"},
		{Name: "commandcode", KeyEnv: "COMMANDCODE_API_KEY",
			Hint: "run `kintsugi connect commandcode` then `kintsugi models fetch commandcode --write` to pick a model"},
	}
}

// DetectCloudProvider returns the first provider whose key is present in the
// environment. The provider's own key env always wins over the store, so a
// stored key is only a fallback.
func DetectCloudProvider(env func(string) string) (CloudProvider, bool) {
	for _, p := range CloudProviders() {
		if env == nil {
			continue
		}
		if strings.TrimSpace(env(p.KeyEnv)) != "" {
			return p, true
		}
	}
	return CloudProvider{}, false
}

// ConfigText is the global config written by setup. The default model comes
// from an open-weights cloud provider; a local runner is only chosen when the
// operator asks for one.
func ConfigText(model string) string {
	provider := ""
	if strings.HasPrefix(model, "ollama/") {
		provider = `
[providers.ollama]
base_url = "http://127.0.0.1:11434/v1"
wire = "openai-completions"
reasoning_effort = "none"   # thinking models otherwise emit reasoning with no answer
`
	}
	modelLine := fmt.Sprintf("default_model = %q", model)
	if model == "" {
		// No model id is known yet: leave the line commented so the harness
		// never starts with a broken default.
		modelLine = `# default_model = "provider/model-id"   # run: kintsugi models fetch <provider> --write`
	}
	return fmt.Sprintf(`# generated by `+"`kintsugi setup`"+` — edit freely.
# Secrets are never stored here: providers name the env var (api_key_env).
# Open-weights cloud providers are the target; local runners are supported
# when you want them (kintsugi setup --local).

[core]
mode = "normal"
%s
%s
[persona_pack]
dir = "~/.config/kintsugi/agents/global"
fallback = "_base"
match_order = ["explicit", "id_glob", "family", "capability", "fallback"]

[behavior]
dir = "~/.config/kintsugi/agents/behavior"
default_profile = "coder-pipeline"

[ui]
banner = true
alt_screen = true

[agents]
subagents = true
max_subagents = 2
`, modelLine, provider)
}

// AgentsTemplate is the global AGENTS.md starter. Workspace AGENTS.md files
// are layered on top of this one.
func AgentsTemplate() string {
	return `# Operator AGENTS.md

This file is injected into every session as operator instructions, on top of
the model-keyed persona pack. Keep it short and factual.

## Workspace rules

- Prefer reading a file's current state over reasoning from memory.
- Do not commit unless explicitly asked; leave the working tree for review.

## Tool preferences

- Use the list/glob/grep/read tools before shell equivalents.
- Run tests with the project's own command before claiming completion.

## Model-specific personas

For doctrine that differs per model family, edit:
  ~/.config/kintsugi/agents/global/<name>.md
with frontmatter such as:
  match:
    family: [qwen]
    id_glob: ["qwen3-coder-*"]
`
}

// WriteFile writes content when the file is absent, or when force is set.
func WriteFile(p, content string, force bool) (string, error) {
	if _, err := os.Stat(p); err == nil && !force {
		return "skipped " + p + " (exists; --force to overwrite)", nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return "", err
	}
	return "wrote " + p, nil
}

// Run performs the full setup into the global config root.
func Run(root, model string, force bool) (Result, error) {
	res := Result{Root: root, Model: model, PackDir: filepath.Join(root, "agents", "global")}
	w, s, err := Materialize(root, force)
	if err != nil {
		return res, err
	}
	res.Written, res.Skipped = w, s
	res.ConfigPath = filepath.Join(root, "config.toml")
	if _, err := WriteFile(res.ConfigPath, ConfigText(model), force); err != nil {
		return res, err
	}
	res.AgentsPath = filepath.Join(root, "AGENTS.md")
	if _, err := WriteFile(res.AgentsPath, AgentsTemplate(), force); err != nil {
		return res, err
	}
	return res, nil
}

// GlobalRoot is ~/.config/kintsugi (the parent of config.GlobalPath).
func GlobalRoot() string { return filepath.Dir(config.GlobalPath()) }

// ModelRef joins a detected Ollama model name into a provider/model ref.
func ModelRef(ollamaModel string) string {
	if strings.TrimSpace(ollamaModel) == "" {
		return ""
	}
	return "ollama/" + ollamaModel
}

// ConfigDirFor is a helper for tests: the directory of a config path.
func ConfigDirFor(p string) string { return path.Dir(filepath.ToSlash(p)) }
