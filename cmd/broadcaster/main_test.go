package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleConfigResolvesSecrets(t *testing.T) {
	t.Setenv("BROADCASTER_API_TOKEN", strings.Repeat("a", 32))
	t.Setenv("BROADCASTER_ADMIN_TOKEN", strings.Repeat("b", 32))
	c, e := readConfig("../../config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Clients) != 2 || !c.Clients[1].Admin || c.Clients[0].Token != os.Getenv("BROADCASTER_API_TOKEN") {
		t.Fatal("incorrect example configuration")
	}
	for _, test := range []struct {
		name, field string
		value       any
	}{
		{"missing networks", "networks", map[string]any{}},
		{"unknown field", "invalid", true},
		{"plaintext webhook", "clients", []map[string]any{{"name": "a", "token": strings.Repeat("x", 32), "webhook": "http://billing.example", "webhookSecret": "short"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, e := os.ReadFile("../../config.example.json")
			if e != nil {
				t.Fatal(e)
			}
			var config map[string]any
			if e = json.Unmarshal(raw, &config); e != nil {
				t.Fatal(e)
			}
			config[test.field] = test.value
			raw, e = json.Marshal(config)
			if e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if e = os.WriteFile(path, raw, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e = readConfig(path); e == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestChainScannerMainnetConfig(t *testing.T) {
	t.Setenv("BROADCASTER_API_TOKEN", strings.Repeat("a", 32))
	t.Setenv("BROADCASTER_ADMIN_TOKEN", strings.Repeat("b", 32))
	const payer = "11111111111111111111111111111111"
	t.Setenv("SOLANA_FEE_PAYER", payer)
	c, err := readConfig("../../config.mainnets.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Networks) != 18 || c.Networks["solana"].FeePayers[0] != payer {
		t.Fatal("mainnet inventory or payer mismatch")
	}
	for _, id := range strings.Fields("1 42161 56 43114 137 10 8453 999 143 5042 9745 4663") {
		if c.Networks[id].Family != "evm" {
			t.Fatal(id)
		}
	}
	for _, network := range strings.Fields("bitcoin tron sui aptos ton solana") {
		if string(c.Networks[network].Family) != network {
			t.Fatal(network)
		}
	}
}
