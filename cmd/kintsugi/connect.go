package main

// `kintsugi connect`: verify and store a provider API key.
//
// The key never reaches stdout, a command line or the config file: it is read
// either from one stdin line (--key-stdin) or through the platform hidden
// prompt, checked against the provider's /models endpoint, and only then
// written to the 0600 credential store. An exported env var still wins at
// request time, so storing a key never overrides the environment (`kintsugi models`
// shows which source is live). A failed verification writes nothing: a typo
// fails here instead of halfway through a task.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"kintsugi/internal/config"
	"kintsugi/internal/credentials"
	"kintsugi/internal/provider"
)

// connectVerifyTimeout bounds the reachability probe. It is deliberately short:
// `kintsugi connect` is an interactive step, not a long task.
const connectVerifyTimeout = 10 * time.Second

// credStore is the slice of internal/credentials that `kintsugi connect` needs.
// The interface exists so the verify-and-save path can be tested against a fake
// store instead of the real on-disk one.
type credStore interface {
	Path() string
	Set(provider, key string) error
	Delete(provider string) error
	Status(provider, envName string) string
}

// diskCreds is the real store.
type diskCreds struct{}

func (diskCreds) Path() string                { return credentials.Path() }
func (diskCreds) Set(p, key string) error     { return credentials.Set(p, key) }
func (diskCreds) Delete(p string) error       { return credentials.Delete(p) }
func (diskCreds) Status(p, env string) string { return credentials.Status(p, env) }

// creds is swapped by tests; production always uses the real store.
var creds credStore = diskCreds{}

// cmdConnect dispatches `kintsugi connect`. Config loading here is deliberately
// config.Load (not kintsugi.New): listing providers must not touch the session
// store, the ledger or any credential file.
func cmdConnect(ws string, args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	list := fs.Bool("list", false, "list configured providers and their key status")
	noVerify := fs.Bool("no-verify", false, "save without pinging the provider first")
	keyStdin := fs.Bool("key-stdin", false, "read the key from one line on stdin")
	del := fs.Bool("delete", false, "remove the stored key for the provider")

	// The usage line puts flags after the provider name, and flag.Parse stops at
	// the first positional argument: split them out first. None of these flags
	// take a value, so a bare "-" prefix is enough to tell them apart.
	var flags, positional []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			continue
		}
		positional = append(positional, a)
	}
	fs.Parse(flags)

	cfg, _, err := config.Load(ws)
	if err != nil {
		fail("%v", err)
	}

	if *list {
		if len(positional) > 0 {
			connectUsage()
		}
		cmdConnectList(cfg, creds)
		return
	}
	if len(positional) != 1 {
		connectUsage()
	}
	name := strings.TrimSpace(positional[0])

	p, err := connectFindProvider(cfg, name)
	if err != nil {
		fail("%v", err)
	}

	// No key required: a loopback runner, or an entry with no api_key_env.
	if !connectNeedsKey(p) {
		if strings.TrimSpace(p.APIKeyEnv) == "" {
			fmt.Printf("%s: no key required (no api_key_env · %s)\n", name, p.BaseURL)
			return
		}
		fmt.Printf("%s: no key required (loopback %s)\n", name, p.BaseURL)
		if !*del {
			return
		}
		// A key stored while the endpoint was remote is still removable after
		// it moved to loopback, so --delete falls through.
	}

	if *del {
		if err := connectDelete(name, creds); err != nil {
			fail("%v", err)
		}
		fmt.Printf("deleted stored key for %s (an exported %s is unaffected)\n", name, p.APIKeyEnv)
		return
	}

	key, err := connectReadKey(name, *keyStdin, os.Stdin)
	if err != nil {
		fail("%v", err)
	}

	if !*noVerify {
		fmt.Fprintf(os.Stderr, "· verifying %s (%s/models, %s)\n",
			name, strings.TrimRight(p.BaseURL, "/"), connectVerifyTimeout)
	}
	// connectApply verifies first and only then writes; a verification failure
	// returns here with nothing stored.
	if err := connectApply(cfg, name, p, key, !*noVerify, creds, connectVerifyTimeout); err != nil {
		fail("%v", err)
	}

	fmt.Printf("saved %s (0600, env still wins)\n", name)
	if v := os.Getenv(p.APIKeyEnv); v != "" {
		fmt.Printf("note: %s is exported in this shell, so it takes precedence over the stored key\n", p.APIKeyEnv)
	}
	fmt.Println()
	for _, ex := range connectExamples(cfg, name, p) {
		fmt.Println("  " + ex)
	}
}

// cmdConnectList prints one row per configured provider. It reads no credential
// file: the status column comes from the env and the store's existence check.
func cmdConnectList(cfg config.File, store credStore) {
	names := cfg.ProviderNames()
	sort.Strings(names)

	// Provider names and endpoints in the shipped registry vary widely
	// ("alibaba-coding-plan" vs "yi"), so size the two wide columns to the data
	// instead of truncating an endpoint the operator needs to read.
	nameW, urlW := len("PROVIDER"), len("BASE URL")
	for _, name := range names {
		nameW = max(nameW, len(name))
		urlW = max(urlW, len(cfg.Providers[name].BaseURL))
	}

	fmt.Printf("%-*s %-*s %-10s %-13s %s\n", nameW, "PROVIDER", urlW, "BASE URL", "FAMILY", "AUTH", "ENV")
	for _, name := range names {
		p := cfg.Providers[name]
		family := p.Family
		if strings.TrimSpace(family) == "" {
			family = "—"
		}
		fmt.Printf("%-*s %-*s %-10s %-13s %s\n",
			nameW, name, urlW, p.BaseURL, family, connectAuthStatus(name, p, store), orDash(p.APIKeyEnv))
	}
	fmt.Printf("\nstored keys: %s (0600)\n", store.Path())
	fmt.Println("`env` beats `stored`: unset the variable to use a stored key. Run `kintsugi connect <provider>` to add one.")
}

// connectUsage is the exit-2 path for a malformed invocation.
func connectUsage() {
	fmt.Fprintln(os.Stderr, `usage: kintsugi connect --list
       kintsugi connect <provider> [--no-verify] [--key-stdin] [--delete]`)
	os.Exit(2)
}

// connectFindProvider resolves one provider by name, naming the configured
// providers when it does not match.
func connectFindProvider(cfg config.File, name string) (config.Provider, error) {
	if name == "" {
		return config.Provider{}, errors.New("no provider given (run `kintsugi connect --list`)")
	}
	if p, ok := cfg.Providers[name]; ok {
		return p, nil
	}
	names := cfg.ProviderNames()
	sort.Strings(names)
	return config.Provider{}, fmt.Errorf("unknown provider %q (configured: %s)", name, strings.Join(names, ", "))
}

// connectNeedsKey reports whether a provider actually authenticates: a loopback
// runner or an entry with no api_key_env takes no key at all.
func connectNeedsKey(p config.Provider) bool {
	if strings.TrimSpace(p.APIKeyEnv) == "" {
		return false
	}
	return !config.IsLoopback(p.BaseURL)
}

// connectAuthStatus is the AUTH column: `env`, `stored`, `missing`, or
// `not required` when the provider takes no key.
func connectAuthStatus(name string, p config.Provider, store credStore) string {
	if !connectNeedsKey(p) {
		return "not required"
	}
	switch store.Status(name, p.APIKeyEnv) {
	case "env":
		return "env"
	case "stored":
		return "stored"
	default:
		return "missing"
	}
}

// connectModelChoice picks the model id used for the verification client: the
// default model when it belongs to this provider, otherwise the first curated
// model, otherwise nothing (Ping only needs the endpoint, not a model id).
func connectModelChoice(cfg config.File, name string, p config.Provider) string {
	if prov, id, ok := strings.Cut(cfg.Core.DefaultModel, "/"); ok && prov == name && strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	if len(p.Models) > 0 && strings.TrimSpace(p.Models[0].ID) != "" {
		return p.Models[0].ID
	}
	return ""
}

// connectVerify builds the provider client, injects the candidate key and pings
// the endpoint. The key stays in memory: the returned error carries the
// provider's own message (status + body), never the key.
func connectVerify(cfg config.File, name string, p config.Provider, key string, timeout time.Duration) error {
	client, err := provider.New(p, connectModelChoice(cfg, name, p), cfg.Core.Mode)
	if err != nil {
		return err
	}
	client.APIKey = key
	if timeout <= 0 {
		timeout = connectVerifyTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.Ping(ctx)
}

// connectApply is the whole mutation: verify (unless disabled), then save. On a
// verification failure the store is never touched, so `--no-verify` is the only
// way to keep a key the endpoint rejected.
func connectApply(cfg config.File, name string, p config.Provider, key string, verify bool, store credStore, timeout time.Duration) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("empty key for %s: nothing was entered (use --key-stdin to pipe one)", name)
	}
	if verify {
		if err := connectVerify(cfg, name, p, key, timeout); err != nil {
			return fmt.Errorf("%s: %v (key not saved; --no-verify saves without checking)", name, err)
		}
	}
	if err := store.Set(name, key); err != nil {
		return fmt.Errorf("save %s: %v", name, err)
	}
	return nil
}

// connectDelete removes a stored key. The provider entry and its env var are
// untouched: dropping a stored key only falls back to the environment.
func connectDelete(name string, store credStore) error {
	if err := store.Delete(name); err != nil {
		return fmt.Errorf("delete %s: %v", name, err)
	}
	return nil
}

// connectReadKey obtains the candidate key. --key-stdin reads exactly one
// trimmed line; otherwise the platform hidden prompt is used.
func connectReadKey(name string, fromStdin bool, stdin io.Reader) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read key from stdin: %v", err)
		}
		return strings.TrimSpace(line), nil
	}
	key, err := credentials.ReadHidden(fmt.Sprintf("key for %s: ", name))
	if err != nil {
		return "", fmt.Errorf("read key: %v (use --key-stdin to pipe it instead)", err)
	}
	return strings.TrimSpace(key), nil
}

// connectExamples are the follow-up commands printed after a save.
func connectExamples(cfg config.File, name string, p config.Provider) []string {
	model := connectModelChoice(cfg, name, p)
	if model == "" {
		model = "<model>"
	}
	return []string{
		fmt.Sprintf("kintsugi run --model %s/%s \"<task>\"", name, model),
		fmt.Sprintf("kintsugi models fetch %s --write", name),
		fmt.Sprintf("kintsugi connect %s --delete", name),
	}
}
