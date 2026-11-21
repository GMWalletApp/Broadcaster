package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/GMWalletApp/Broadcaster/internal/broadcast"
	"github.com/ethereum/go-ethereum/common"
	sol "github.com/gagliardetto/solana-go"
)

func readConfig(path string) (broadcast.Config, error) {
	var c broadcast.Config
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	// Accept the retired scanner key in existing configurations without loading
	// credentials, validating a consumer, or starting any scanner work.
	var config struct {
		*broadcast.Config
		LegacyScanner json.RawMessage `json:"scanner"`
	}
	config.Config = &c
	if e = d.Decode(&config); e != nil {
		return c, e
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("unexpected trailing configuration")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if len(c.Networks) == 0 || len(c.Clients) == 0 {
		return c, errors.New("configure at least one network and one client")
	}
	for name, n := range c.Networks {
		if n.PublicNodeProbeFile != "" && !filepath.IsAbs(n.PublicNodeProbeFile) {
			n.PublicNodeProbeFile = filepath.Join(filepath.Dir(path), n.PublicNodeProbeFile)
			c.Networks[name] = n
		}
		if n.MaxHeightLag < 0 || n.StallSeconds < 0 {
			return c, fmt.Errorf("invalid health thresholds for %s", name)
		}
		switch n.Family {
		case broadcast.EVM:
			id, e := strconv.ParseUint(name, 10, 64)
			if e != nil || id == 0 || name != strconv.FormatUint(id, 10) {
				return c, fmt.Errorf("invalid EVM chain ID %q", name)
			}
			for _, a := range n.EntryPoints {
				if !common.IsHexAddress(a) {
					return c, errors.New("invalid EntryPoint address")
				}
			}
		case broadcast.Solana:
			if _, e := sol.HashFromBase58(n.GenesisHash); e != nil || name == "" || len(n.FeePayers) == 0 {
				return c, errors.New("configure cluster genesis and fee payers")
			}
			for i, p := range n.FeePayers {
				p = os.ExpandEnv(p)
				n.FeePayers[i] = p
				if _, e := sol.PublicKeyFromBase58(p); e != nil {
					return c, errors.New("invalid fee payer")
				}
			}
		default:
			if !broadcast.IsNative(n.Family) || name != string(n.Family) {
				return c, fmt.Errorf("invalid family for network %s", name)
			}
		}
	}
	names, tokens := map[string]bool{}, map[string]bool{}
	for i := range c.Clients {
		client := &c.Clients[i]
		client.Token = os.ExpandEnv(client.Token)
		client.WebhookSecret = os.ExpandEnv(client.WebhookSecret)
		if client.Name == "" || names[client.Name] || tokens[client.Token] || len(client.Token) < 24 {
			return c, errors.New("unique client names and tokens of at least 24 bytes required")
		}
		names[client.Name] = true
		tokens[client.Token] = true
		if client.Webhook != "" {
			u, e := url.Parse(client.Webhook)
			if e != nil || u.Scheme != "https" || u.Host == "" || len(client.WebhookSecret) < 24 {
				return c, errors.New("webhook requires HTTPS and a signing secret of at least 24 bytes")
			}
		}
	}
	return c, nil
}

func run() error {
	path := flag.String("config", "config.json", "configuration file")
	dbPath := flag.String("db", "broadcaster.db", "SQLite database file")
	flag.Parse()
	c, e := readConfig(*path)
	if e != nil {
		return e
	}
	key, e := base64.StdEncoding.DecodeString(os.Getenv("BROADCASTER_STORAGE_KEY"))
	if e != nil || len(key) != 32 {
		return errors.New("set BROADCASTER_STORAGE_KEY to a base64-encoded 32-byte key")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, e := broadcast.Open(ctx, *dbPath)
	if e != nil {
		return e
	}
	defer st.Close()
	svc, e := broadcast.New(c, st, key)
	if e != nil {
		return e
	}
	slog.Info("serving broadcaster", "address", c.Listen)
	return svc.Serve(ctx)
}

func main() {
	if e := run(); e != nil {
		slog.Error("broadcaster stopped", "error", e)
		os.Exit(1)
	}
}
