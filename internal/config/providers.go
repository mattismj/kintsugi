package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// providers.json is the first-party, versioned provider registry. It is
// deliberately data, not code: `kintsugi models` and the importer read the same
// shape, and operators override any field in their own config. External
// catalogs (for example oh-my-pi's) are imported separately; none of their
// data is bundled here.
//
//go:embed providers.json
var providersJSON []byte

type providerRegistry struct {
	Version   int                 `json:"version"`
	Providers map[string]Provider `json:"providers"`
}

func loadProviderRegistry() providerRegistry {
	var reg providerRegistry
	if err := json.Unmarshal(providersJSON, &reg); err != nil {
		panic(fmt.Sprintf("config: embedded provider registry is invalid: %v", err))
	}
	if len(reg.Providers) == 0 {
		panic("config: embedded provider registry is empty")
	}
	return reg
}

func defaultProviders() map[string]Provider { return loadProviderRegistry().Providers }

// RegistryVersion identifies the embedded registry for manifests and reports.
func RegistryVersion() int { return loadProviderRegistry().Version }
