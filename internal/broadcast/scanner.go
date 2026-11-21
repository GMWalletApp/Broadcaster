package broadcast

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ScannerConfig uses a dedicated consumer because ChainScanner replaces its
// complete subscription list. The feed is a wake-up hint, never chain evidence.
type ScannerConfig struct {
	URL      string            `json:"url"`
	Token    string            `json:"token"`
	Networks map[string]string `json:"networks"` // local EVM chain ID -> scanner network ID
}

func (s *Service) scannerCall(ctx context.Context, method, path string, body []byte, dst any) (int, error) {
	cfg := s.cfg.Scanner
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(cfg.URL, "/")+path, bytes.NewReader(body))
	if e != nil {
		return 0, errors.New("invalid scanner request")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, e := s.http.Do(req)
	if e != nil {
		return 0, errors.New("scanner unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("scanner HTTP %d", resp.StatusCode)
	}
	if dst != nil && resp.StatusCode != 204 {
		e = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(dst)
	}
	return resp.StatusCode, e
}

func (s *Service) syncWatches(ctx context.Context) error {
	rows, e := s.Store.db.QueryContext(ctx, "SELECT DISTINCT network,sender FROM tasks WHERE family='evm' AND state<>'CLOSED' ORDER BY network,sender")
	if e != nil {
		return e
	}
	list := []map[string]string{}
	for rows.Next() {
		var n, a string
		if e = rows.Scan(&n, &a); e != nil {
			rows.Close()
			return e
		}
		if net := s.cfg.Scanner.Networks[n]; net != "" {
			list = append(list, map[string]string{"network": net, "address": a})
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	_, e = s.scannerCall(ctx, "PUT", "/v1/subscriptions", bodyJSON(list), nil)
	return e
}

func (s *Service) nextActivity(ctx context.Context, chain, network string) (bool, error) {
	var delivery struct {
		ID       int64 `json:"id"`
		Activity struct {
			Network   string   `json:"network"`
			Addresses []string `json:"addresses"`
		} `json:"activity"`
	}
	code, e := s.scannerCall(ctx, "GET", "/v1/events/next?network="+url.QueryEscape(network), nil, &delivery)
	if e != nil || code == 204 {
		return false, e
	}
	if delivery.ID <= 0 || delivery.Activity.Network != network {
		return false, errors.New("invalid scanner delivery")
	}
	e = s.Store.atomic(ctx, func(tx *sql.Tx) error {
		for _, a := range delivery.Activity.Addresses {
			_, e := tx.ExecContext(ctx, "UPDATE tasks SET due=0 WHERE network=? AND sender=? AND state<>'CLOSED'", chain, strings.ToLower(a))
			if e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		return false, e
	}
	_, e = s.scannerCall(ctx, "POST", fmt.Sprintf("/v1/events/%d/ack", delivery.ID), nil, nil)
	return true, e
}

func (s *Service) scan(ctx context.Context) {
	var syncAt time.Time
	for ctx.Err() == nil {
		if time.Now().After(syncAt) {
			if e := s.syncWatches(ctx); e != nil {
				slog.Warn("scanner subscriptions deferred", "error", e)
			}
			syncAt = time.Now().Add(15 * time.Second)
		}
		worked := false
		for chain, network := range s.cfg.Scanner.Networks {
			ok, e := s.nextActivity(ctx, chain, network)
			if e != nil {
				slog.Debug("scanner delivery deferred", "error", e)
			}
			worked = worked || ok
		}
		if !worked && !wait(ctx, time.Second) {
			return
		}
	}
}
