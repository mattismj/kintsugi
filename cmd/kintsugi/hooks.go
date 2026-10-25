package main

// `kintsugi hooks` — the trust and inspection surface for lifecycle hooks
// (M9.1).
//
// Two sources of hook rules exist and they are not equally trusted:
//
//   - operator rules: the [hooks] section of the operator's global config
//     (~/.config/kintsugi/config.toml). The operator wrote them for this
//     machine; they run without a marker.
//   - project rules: <ws>/.kintsugi/hooks.toml, inside the repository. A
//     cloned repository ships that file, so its rules are commands the
//     repository chooses to run on the operator's machine. They run only in a
//     workspace the operator marked as trusted with `kintsugi hooks trust`.
//
// The marker is a permission, not a rule: it enables the project's hooks, and
// only the ones that exist.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"kintsugi/internal/config"
	"kintsugi/internal/hooks"
)

// cmdHooks is the `kintsugi hooks` entry point: trust | list | status.
func cmdHooks(ws string, args []string) {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "trust":
		cmdHooksTrust(ws)
	case "untrust":
		cmdHooksUntrust(ws)
	case "list":
		cmdHooksList(ws)
	case "status":
		cmdHooksStatus(ws)
	default:
		fail("usage: kintsugi hooks trust|untrust|list|status")
	}
}

// cmdHooksTrust writes the workspace trust marker and says what it enables.
func cmdHooksTrust(ws string) {
	if err := hooks.Trust(ws); err != nil {
		fail("%v", err)
	}
	fmt.Printf("trusted %s\n", ws)
	fmt.Printf("marker  %s\n", hooks.TrustedMarkerPath(ws))
	fmt.Printf("written %s\n", hooks.TrustMarkerTime(ws).Format(time.RFC3339))
	fmt.Println()
	fmt.Printf("This lets the project hooks in %s run as commands on this machine.\n",
		hooks.ProjectHooksPath(ws))
	fmt.Println("Review them first: kintsugi hooks list")
	fmt.Println("Revoke with: kintsugi hooks untrust")
}

// cmdHooksUntrust removes the workspace trust marker.
func cmdHooksUntrust(ws string) {
	if err := hooks.Untrust(ws); err != nil {
		fail("%v", err)
	}
	fmt.Printf("untrusted %s\n", ws)
	fmt.Printf("marker  %s (removed)\n", hooks.TrustedMarkerPath(ws))
	fmt.Println("Project hooks are skipped again.")
}

// cmdHooksStatus reports the trust state of the workspace and where the
// marker lives.
func cmdHooksStatus(ws string) {
	marker := hooks.TrustedMarkerPath(ws)
	project := hooks.ProjectHooksPath(ws)

	fmt.Printf("workspace      %s\n", ws)
	fmt.Printf("project hooks  %s%s\n", project, absentSuffix(project))
	fmt.Printf("trust marker   %s%s\n", marker, absentSuffix(marker))
	if hooks.IsTrusted(ws) {
		fmt.Printf("trusted        yes — marker written %s\n", hooks.TrustMarkerTime(ws).Format(time.RFC3339))
	} else {
		fmt.Println("trusted        no — project hooks are skipped; run `kintsugi hooks trust` to enable them")
	}
}

// cmdHooksList prints every event with the operator's rules and the
// workspace's project rules, and the trust state the project rules run under.
func cmdHooksList(ws string) {
	operator, operatorSrc, err := operatorHookRules()
	if err != nil {
		fail("%v", err)
	}
	project, trusted, err := hooks.LoadProjectHooks(ws)
	if err != nil {
		fail("%v", err)
	}

	fmt.Println("hooks")
	fmt.Printf("  operator  %s\n", operatorSrc)
	fmt.Printf("  project   %s\n", hooks.ProjectHooksPath(ws))
	if trusted {
		fmt.Println("  trust     trusted — project hooks run")
	} else {
		fmt.Println("  trust     untrusted — project hooks are skipped (run `kintsugi hooks trust`)")
	}
	fmt.Println()

	for _, ev := range hooks.Events {
		op := operator[ev]
		pr := project[ev]
		fmt.Println(ev)
		if len(op) == 0 && len(pr) == 0 {
			fmt.Println("  (none)")
			continue
		}
		for _, rule := range op {
			printHookRule("operator", rule, "")
		}
		for _, rule := range pr {
			note := ""
			if !trusted {
				note = "  [skipped: workspace is not trusted]"
			}
			printHookRule("project", rule, note)
		}
	}
}

// operatorHookRules loads the operator's (global) hooks and the paths they
// came from.
//
// Only the global config counts as operator configuration here: the
// workspace's own files are repository-controlled, and presenting them as
// "operator" rules would hide the trust question they raise.
func operatorHookRules() (map[string][]config.HookRule, string, error) {
	cfg, loaded, err := config.Load("")
	if err != nil {
		return nil, "", err
	}
	src := config.GlobalPath()
	if len(loaded) > 0 {
		src = strings.Join(loaded, " ")
	}
	return hooks.FromConfig(cfg.Hooks), src, nil
}

// printHookRule prints one rule: its matcher, the effective timeout, the
// async flag and the command. The note carries the trust state for a project
// rule that would be skipped.
func printHookRule(source string, rule config.HookRule, note string) {
	matcher := strings.TrimSpace(rule.Matcher)
	if matcher == "" {
		matcher = "*"
	}
	fmt.Printf("  %-9s matcher=%-14s timeout=%-5d async=%-4s command=%s%s\n",
		source, matcher, hooks.EffectiveTimeout(rule), asyncLabel(rule.Async), rule.Command, note)
}

// asyncLabel renders the async flag.
func asyncLabel(async bool) string {
	if async {
		return "yes"
	}
	return "no"
}

// absentSuffix marks a path that does not exist.
func absentSuffix(path string) string {
	if _, err := os.Stat(path); err != nil {
		return "  (absent)"
	}
	return ""
}
