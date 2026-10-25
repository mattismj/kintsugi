package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"kintsugi/internal/setup"
)

func cmdSetup(ws string, args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	model := fs.String("model", "", "provider/model (default: an open-weights provider whose key is exported)")
	local := fs.Bool("local", false, "prefer a local runner (Ollama/vLLM) instead of a cloud provider")
	force := fs.Bool("force", false, "overwrite existing config and AGENTS.md")
	fs.Parse(args)

	chosen := strings.TrimSpace(*model)
	source := ""
	hint := ""
	if chosen == "" && *local {
		if m := setup.DetectOllama(2 * time.Second); m != "" {
			chosen = setup.ModelRef(m)
			source = "local runner detected"
		}
	}
	if chosen == "" {
		if p, ok := setup.DetectCloudProvider(os.Getenv); ok {
			source = p.Name + " (key found in " + p.KeyEnv + ")"
			chosen = p.Model
			if p.Model == "" {
				hint = p.Hint
			}
		}
	}
	if chosen == "" {
		chosen = "deepseek/deepseek-chat"
		source = "default — export a provider key or run `kintsugi connect <provider>`"
	}

	root := setup.GlobalRoot()
	res, err := setup.Run(root, chosen, *force)
	if err != nil {
		fail("setup: %v", err)
	}
	fmt.Printf("kintsugi setup · %s\n\n", root)
	fmt.Printf("model    %s\n", chosen)
	fmt.Printf("source   %s\n", source)
	if hint != "" {
		fmt.Printf("hint     %s\n", hint)
	}
	fmt.Printf("pack     %s\n", res.PackDir)
	for _, w := range res.Written {
		fmt.Println("wrote    " + w)
	}
	for _, s := range res.Skipped {
		fmt.Println("skipped  " + s)
	}
	fmt.Println("\nNext:")
	fmt.Println("  1. edit " + res.AgentsPath + " — operator rules applied to every session")
	fmt.Println("  2. model-specific doctrine: edit a persona in " + res.PackDir)
	fmt.Println("  3. kintsugi connect <provider>   sign in to an open-weights provider")
	fmt.Println("  4. kintsugi doctor               environment check")
	fmt.Println("  5. kintsugi                      interactive TUI")
	fmt.Println("     kintsugi run \"...\"            one-shot task")
}
