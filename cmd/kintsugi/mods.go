package main

// `kintsugi mods` — the inspection, trust and debugging surface for the
// constrained mod API (M12.5).
//
// A mod is an external process that contributes tools and slash commands.
// The mods live in the workspace (.kintsugi/mods by default), so they are
// repository-controlled code: they run only in a workspace the operator
// marked as trusted. The marker is the same one the project hooks use, so
// one decision governs both surfaces.
//
// The command never starts a mod by itself, except for `kintsugi mods call`,
// which starts exactly one mod to answer a single tool call.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"kintsugi/internal/config"
	"kintsugi/internal/hooks"
	"kintsugi/internal/mods"
)

// cmdMods is the `kintsugi mods` entry point: list | trust | untrust | call.
func cmdMods(ws string, args []string) {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		cmdModsList(ws)
	case "trust":
		cmdModsTrust(ws)
	case "untrust":
		cmdModsUntrust(ws)
	case "call":
		cmdModsCall(ws, args[1:])
	default:
		fail("usage: kintsugi mods list|trust|untrust|call <mod> <tool> <json>")
	}
}

// modsConfig loads the configuration and resolves the mods directory.
func modsConfig(ws string) (config.Mods, string) {
	cfg, _, err := config.Load(ws)
	if err != nil {
		fail("%v", err)
	}
	return cfg.Mods, mods.Dir(ws, cfg.Mods.Dir)
}

// cmdModsList prints every manifest it finds, the trust state it would run
// under, and what each mod provides.
func cmdModsList(ws string) {
	cfg, dir := modsConfig(ws)
	manifests, err := mods.Load(dir)
	if err != nil {
		// A broken manifest is reported, but the working ones are still
		// listed.
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}

	fmt.Println("mods")
	fmt.Printf("  directory  %s%s\n", dir, absentSuffix(dir))
	if cfg.Enabled {
		fmt.Println("  enabled    yes — mods are looked for in the directory")
	} else {
		fmt.Println("  enabled    no — set [mods] enabled = true to look for mods")
	}
	if hooks.IsTrusted(ws) {
		fmt.Println("  trust      trusted — project mods run")
	} else {
		fmt.Println("  trust      untrusted — project mods are skipped (run `kintsugi mods trust`)")
	}
	fmt.Println()

	if len(manifests) == 0 {
		fmt.Println("(no mod manifests)")
		return
	}
	for _, mf := range manifests {
		fmt.Printf("%s", mf.Name)
		if mf.Version != "" {
			fmt.Printf("  %s", mf.Version)
		}
		fmt.Println()
		if mf.Description != "" {
			fmt.Printf("  description  %s\n", mf.Description)
		}
		fmt.Printf("  command      %s %s\n", mf.Command, strings.Join(mf.Args, " "))
		fmt.Printf("  tools        %s\n", toolList(mf))
		fmt.Printf("  commands     %s\n", commandList(mf))
		if len(mf.Events) > 0 {
			fmt.Printf("  events       %s\n", strings.Join(mf.Events, ", "))
		}
		if len(mf.Env) > 0 {
			fmt.Printf("  env          %s\n", envList(mf))
		}
		if hooks.IsTrusted(ws) {
			fmt.Println("  state        will run (the workspace is trusted)")
		} else {
			fmt.Println("  state        listed only — the workspace is not trusted")
		}
		fmt.Println()
	}
}

// cmdModsTrust writes the workspace trust marker and says what it enables.
func cmdModsTrust(ws string) {
	if err := hooks.Trust(ws); err != nil {
		fail("%v", err)
	}
	fmt.Printf("trusted %s\n", ws)
	fmt.Printf("marker  %s\n", hooks.TrustedMarkerPath(ws))
	fmt.Printf("written %s\n", hooks.TrustMarkerTime(ws).Format(time.RFC3339))
	fmt.Println()
	fmt.Println("This lets the project hooks and the project mods in this workspace run as")
	fmt.Println("commands on this machine.")
	fmt.Println("Review them first: kintsugi hooks list / kintsugi mods list")
	fmt.Println("Revoke with: kintsugi mods untrust")
}

// cmdModsUntrust removes the workspace trust marker.
func cmdModsUntrust(ws string) {
	if err := hooks.Untrust(ws); err != nil {
		fail("%v", err)
	}
	fmt.Printf("untrusted %s\n", ws)
	fmt.Printf("marker  %s (removed)\n", hooks.TrustedMarkerPath(ws))
	fmt.Println("Project mods are skipped again.")
}

// cmdModsCall starts one mod and calls one of its tools. It is the
// debugging path: the same wire the harness uses, without a session.
func cmdModsCall(ws string, args []string) {
	if len(args) < 2 {
		fail("usage: kintsugi mods call <mod> <tool> <json>")
	}
	name := args[0]
	tool := args[1]
	arguments := "{}"
	if len(args) > 2 {
		arguments = strings.Join(args[2:], " ")
	}

	_, dir := modsConfig(ws)
	manifests, err := mods.Load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}
	var found *mods.Manifest
	for i := range manifests {
		if manifests[i].Name == name {
			found = &manifests[i]
			break
		}
	}
	if found == nil {
		fail("no mod named %q in %s", name, dir)
	}
	if !mods.Trusted(ws) {
		fail("mod %q: %v", name, mods.ErrUntrusted)
	}

	mgr := mods.NewManager(30*time.Second, nil)
	defer mgr.Close()
	if err := mgr.Discover([]mods.Manifest{*found}, true); err != nil {
		fail("%v", err)
	}
	out, isErr, err := mgr.Call(context.Background(), tool, arguments)
	if err != nil {
		fail("%v", err)
	}
	fmt.Println(out)
	if isErr {
		os.Exit(1)
	}
}

// toolList renders the tools a manifest declares.
func toolList(mf mods.Manifest) string {
	if len(mf.Tools) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(mf.Tools))
	for _, t := range mf.Tools {
		names = append(names, t.Name)
	}
	return strings.Join(names, ", ")
}

// commandList renders the slash commands a manifest declares.
func commandList(mf mods.Manifest) string {
	if len(mf.Commands) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(mf.Commands))
	for _, c := range mf.Commands {
		name := c.Name
		if c.ArgumentHint != "" {
			name += " " + c.ArgumentHint
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// envList renders the environment variables a manifest names. Only the names
// are ever shown: the values stay in the environment.
func envList(mf mods.Manifest) string {
	if len(mf.Env) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(mf.Env))
	for name, src := range mf.Env {
		if src == "" {
			src = name
		}
		names = append(names, name+"←"+src)
	}
	return strings.Join(names, ", ")
}
