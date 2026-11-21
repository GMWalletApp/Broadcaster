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
