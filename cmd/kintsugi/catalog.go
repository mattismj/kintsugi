package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"kintsugi/internal/catalog"
	"kintsugi/internal/config"
)

func cmdModelsImport(ws string, args []string) {
	fs := flag.NewFlagSet("models import", flag.ExitOnError)
	write := fs.Bool("write", false, "persist the normalized catalog to the configured catalog path")
	asJSON := fs.Bool("json", false, "print the normalized catalog")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		fail("usage: kintsugi models import <file|-> [--write] [--json]")
	}
	// Allow flags after the file name: flag.Parse stops at the first
	// positional, so re-parse anything that follows it.
	src := rest[0]
	if len(rest) > 1 {
		fs.Parse(rest[1:])
	}
	var raw []byte
	var err error
	if src == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(src)
	}
	if err != nil {
		fail("models import: %v", err)
	}
	cat, err := catalog.Parse(raw)
	if err != nil {
		fail("models import: %v", err)
	}

	if *asJSON {
		b, _ := json.MarshalIndent(cat, "", "  ")
		fmt.Println(string(b))
	}

	app := newApp(ws)
	added := catalog.MergeInto(&app.Cfg, cat)
	total, known := 0, 0
	for name, p := range cat.Providers {
		total += len(p.Models)
		if _, ok := app.Cfg.Providers[name]; ok {
			known++
		}
	}
	fmt.Printf("catalog v%d · %d provider(s) · %d model(s) · %d matched to configured providers · %d new\n",
		cat.Version, len(cat.Providers), total, known, added)
	for name, p := range cat.Providers {
		note := "configured"
		if _, ok := app.Cfg.Providers[name]; !ok {
			note = "no provider config (models will not be usable until one exists)"
		}
		fmt.Printf("  %-12s %3d models  %s\n", name, len(p.Models), note)
	}

	if *write {
		path := app.Cfg.Core.CatalogPath
		if path == "" {
			path = config.ExpandHome("~/.config/kintsugi/catalog.json")
		}
		if err := catalog.Save(path, cat); err != nil {
			fail("models import: %v", err)
		}
		fmt.Printf("\nwrote %s\n", path)
	} else {
		fmt.Println("\nreport only; pass --write to persist (applied at next start)")
	}
}

// cmdModelsFetch reads a provider's own /models endpoint (WP-4.6) instead of a
// catalog file. Egress is the operator's explicit choice here, so unlike the
// agent loop this command does not consult core.mode: `kintsugi models fetch` is a
// thing you run on purpose.
func cmdModelsFetch(ws string, args []string) {
	fs := flag.NewFlagSet("models fetch", flag.ExitOnError)
	write := fs.Bool("write", false, "merge the fetched models into the cached catalog")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request timeout")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		fail("usage: kintsugi models fetch <provider> [--write] [--timeout 30s]")
	}
	// Allow flags after the provider name: flag.Parse stops at the first
	// positional, so re-parse anything that follows it.
	name := rest[0]
	if len(rest) > 1 {
		fs.Parse(rest[1:])
	}

	app := newApp(ws)
	p, err := catalog.LookupProvider(app.Cfg, name)
	if err != nil {
		fail("models fetch: %v", err)
	}
	models, err := catalog.Fetch(context.Background(), p, *timeout)
	if err != nil {
		fail("models fetch: %v", err)
	}

	fmt.Printf("%s · %s · %d model(s)\n", name, p.BaseURL, len(models))
	for _, m := range models {
		fam := m.Family
		if fam == "" {
			fam = p.Family
		}
		fmt.Printf("  %-42s family=%-10s", m.ID, orDash(fam))
		if m.Context > 0 {
			fmt.Printf(" ctx=%d", m.Context)
		}
		if m.Reasoning {
			fmt.Printf(" reasoning")
		}
		fmt.Println()
	}

	cat := &catalog.Catalog{Version: catalog.Version, Providers: map[string]catalog.Provider{}}
	mergeFetchedModels(cat, name, models)
	added := catalog.MergeInto(&app.Cfg, cat)
	if !*write {
		fmt.Printf("\n%d new · report only; pass --write to persist (applied at next start)\n", added)
		return
	}
	path := app.Cfg.Core.CatalogPath
	if path == "" {
		path = config.ExpandHome("~/.config/kintsugi/catalog.json")
	}
	// Merge into whatever is already cached so fetching one provider never
	// drops another provider's models.
	merged := loadCachedCatalog(path)
	mergeFetchedModels(merged, name, models)
	if err := catalog.Save(path, merged); err != nil {
		fail("models fetch: %v", err)
	}
	fmt.Printf("\n%d new · wrote %s\n", added, path)
}

// loadCachedCatalog reads the on-disk catalog; a missing or unreadable file
// starts an empty one (fetch --write may legitimately create the first file).
func loadCachedCatalog(path string) *catalog.Catalog {
	if b, err := os.ReadFile(path); err == nil {
		if cat, err := catalog.Parse(b); err == nil {
			return cat
		}
	}
	return &catalog.Catalog{Version: catalog.Version, Providers: map[string]catalog.Provider{}}
}

// mergeFetchedModels adds models for one provider without overwriting ids that
// are already cached.
func mergeFetchedModels(dst *catalog.Catalog, provider string, models []config.Model) {
	if dst.Providers == nil {
		dst.Providers = map[string]catalog.Provider{}
	}
	entry := dst.Providers[provider]
	have := map[string]bool{}
	for _, m := range entry.Models {
		have[m.ID] = true
	}
	for _, m := range models {
		if have[m.ID] {
			continue
		}
		entry.Models = append(entry.Models, catalog.Model{
			ID: m.ID, Family: m.Family, Context: m.Context,
			Reasoning: m.Reasoning, Capabilities: m.Capabilities,
		})
		have[m.ID] = true
	}
	sort.Slice(entry.Models, func(i, j int) bool { return entry.Models[i].ID < entry.Models[j].ID })
	dst.Providers[provider] = entry
}
