package broadcast

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

const endpointCols = "id,provider,network,secret,enabled,height,healthy,checked,progressed,failures,level,cooldown,probe_until,last_error,generation,config_version"

func (s *Service) endpoint(row scanner) (Endpoint, error) {
	var p Endpoint
	var sealed []byte
	var checked, progressed int64
	var cool, probe sql.NullInt64
	err := row.Scan(&p.ID, &p.Provider, &p.Network, &sealed, &p.Enabled, &p.Height, &p.Healthy, &checked, &progressed, &p.Failures, &p.Level, &cool, &probe, &p.LastError, &p.generation, &p.configVersion)
	if err != nil {
		return p, err
	}
	p.Checked = time.UnixMilli(checked)
	p.Progressed = time.UnixMilli(progressed)
	if cool.Valid {
		v := time.UnixMilli(cool.Int64)
		p.Cooldown = &v
	}
	if probe.Valid {
		v := time.UnixMilli(probe.Int64)
		p.Probe = &v
	}
	sec, err := s.box.Open(nil, nil, sealed, []byte(p.ID))
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(sec, &p.secret)
	if err == nil {
		s.identifyRoute(&p)
	}
	return p, err
}

func (s *Service) endpoints(ctx context.Context, network string, readable bool) ([]Endpoint, error) {
	q := "SELECT " + endpointCols + " FROM providers WHERE (?1='' OR network=?1)"
	if readable {
		q += " AND enabled AND healthy AND checked>(unixepoch()-120)*1000"
	}
	rows, e := s.Store.db.QueryContext(ctx, q+" ORDER BY height DESC,id", network)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	ps := []Endpoint{}
	for rows.Next() {
		p, e := s.endpoint(rows)
		if e != nil {
			return nil, e
		}
		ps = append(ps, p)
	}
	return ps, rows.Err()
}

type ProviderInput struct {
	Network  string  `json:"network"`
	Chain    string  `json:"chainId"`
	Provider string  `json:"providerId"`
	URL      *string `json:"url"`
	Bearer   *string `json:"bearer"`
	Enabled  *bool   `json:"enabled"`
	// Admin-supplied signed probes are real submissions. They are never logged.
	NativeProbe *NativeRequest `json:"nativeProbe"`
	EVMProbes   []string       `json:"evmProbes"`
	SolanaProbe *SolanaRequest `json:"solanaProbe"`
}

func (s *Service) height(ctx context.Context, p Endpoint) (int64, error) {
	if a := nativeAdapters[s.cfg.Networks[p.Network].Family]; a != nil {
		return a.height(ctx, s, p)
	}
	if s.cfg.Networks[p.Network].Family == Solana {
		var h *int64
		e := s.call(ctx, p, "getBlockHeight", []any{map[string]string{"commitment": "confirmed"}}, &h)
		if e == nil && (h == nil || *h < 0) {
			e = errors.New("missing block height")
		}
		if e != nil {
			return 0, e
		}
		return *h, nil
	}
	var h hexutil.Uint64
	e := s.call(ctx, p, "eth_blockNumber", []any{}, &h)
	return int64(h), e
}

func (s *Service) admit(ctx context.Context, p Endpoint, input ProviderInput) error {
	n := s.cfg.Networks[p.Network]
	if a := nativeAdapters[n.Family]; a != nil {
		if n.Family == Bitcoin {
			var indexes map[string]struct{ Synced bool }
			if err := s.call(ctx, p, "getindexinfo", []any{"txindex"}, &indexes); err != nil || !indexes["txindex"].Synced {
				return invalid("Bitcoin requires a synced txindex")
			}
		}
		if input.NativeProbe == nil {
			return invalid("signed nativeProbe required")
		}
		v, err := decodeNative(n.Family, *input.NativeProbe)
		if err != nil {
			return err
		}
		if _, err = a.lookup(ctx, s, p, v.Hash); err != nil {
			return invalid("native transaction lookup unavailable")
		}
		r := s.send(ctx, p, n.Family, v)
		if r.state != "ACCEPTED" && r.state != "ALREADY_KNOWN" {
			return invalid("native broadcast probe failed: " + r.detail)
		}
		return nil
	}
	if n.Family == EVM {
		var chain hexutil.Big
		if e := s.call(ctx, p, "eth_chainId", []any{}, &chain); e != nil || chain.ToInt().String() != p.Network {
			return invalid("eth_chainId mismatch or inaccessible")
		}
		probes := map[string]bool{}
		var verified []Version
		for _, raw := range input.EVMProbes {
			t, v, e := decodeEVM(raw)
			if e != nil || t.Network != p.Network {
				return invalid("invalid admission probe")
			}
			b, _ := hexutil.Decode(raw)
			var tx types.Transaction
			if e = tx.UnmarshalBinary(b); e != nil {
				return invalid("invalid probe encoding")
			}
			if tx.Type() == types.SetCodeTxType {
				probes["type4"] = true
			} else {
				probes["ordinary"] = true
			}
			if to := tx.To(); to != nil && len(tx.Data()) >= 4 {
				selector := hexutil.Encode(tx.Data()[:4])
				if slices.Contains([]string{"0x765e827f", "0x1fad948c", "0x4b1d7cf5", "0xdbed18e0"}, selector) {
					probes[strings.ToLower(to.Hex())] = true
				}
			}
			verified = append(verified, v)
		}
		if !probes["ordinary"] {
			return invalid("ordinary signed admission probe required")
		}
		if supportsSetCode(p.Network) && !probes["type4"] {
			return invalid("ordinary and Type 4 signed admission probes required")
		}
		for _, addr := range n.EntryPoints {
			var code string
			if !probes[strings.ToLower(addr)] || s.call(ctx, p, "eth_getCode", []any{addr, "latest"}, &code) != nil || len(code) <= 2 {
				return invalid("EntryPoint code and bundle probe required")
			}
		}
		var receipt json.RawMessage
		if _, e := s.nonceAt(ctx, p, "0x0000000000000000000000000000000000000000", "latest"); e != nil {
			return invalid("nonce query permission required")
		}
		if _, e := s.block(ctx, p, "latest", false); e != nil {
			return invalid("canonical block queries required")
		}
		if e := s.call(ctx, p, "eth_getTransactionReceipt", []any{verified[0].Hash}, &receipt); e != nil {
			return invalid("receipt permission check failed: " + e.Error())
		}
		for _, v := range verified {
			r := s.send(ctx, p, EVM, v)
			if r.state != "ACCEPTED" && r.state != "ALREADY_KNOWN" {
				return invalid("broadcast admission failed: " + r.detail)
			}
		}
	} else {
		var genesis string
		if s.call(ctx, p, "getGenesisHash", []any{}, &genesis) != nil || genesis != n.GenesisHash {
			return invalid("cluster genesis mismatch")
		}
		if input.SolanaProbe == nil || input.SolanaProbe.Cluster != p.Network {
			return invalid("signed cluster probe required")
		}
		_, v, e := decodeSolana(*input.SolanaProbe, n)
		if e != nil {
			return e
		}
		var first uint64
		if e = s.call(ctx, p, "getFirstAvailableBlock", []any{}, &first); e != nil {
			return invalid("history access required")
		}
		var slot uint64
		if e = s.call(ctx, p, "getSlot", []any{map[string]string{"commitment": "finalized"}}, &slot); e != nil {
			return invalid("finalized slot query required")
		}
		var block *struct {
			Hash       string   `json:"blockhash"`
			Height     *uint64  `json:"blockHeight"`
			Signatures []string `json:"signatures"`
		}
		if e = s.call(ctx, p, "getBlock", []any{slot, map[string]any{"commitment": "finalized", "transactionDetails": "signatures", "rewards": false, "maxSupportedTransactionVersion": 0}}, &block); e != nil || block == nil || block.Hash == "" || block.Height == nil || block.Signatures == nil {
			return invalid("finalized block history required")
		}
		var validity struct {
			Value *bool `json:"value"`
		}
		if e = s.call(ctx, p, "isBlockhashValid", []any{v.Blockhash, map[string]any{"commitment": "confirmed", "minContextSlot": v.ContextSlot}}, &validity); e != nil || validity.Value == nil || !*validity.Value {
			return invalid("fresh recent blockhash required")
		}
		if _, e = s.solStatus(ctx, p, v.Hash); e != nil {
			return invalid("signature status access required")
		}
		r := s.send(ctx, p, Solana, v)
		if r.state != "ACCEPTED" && r.state != "ALREADY_KNOWN" {
			return invalid("Solana admission failed: " + r.detail)
		}
	}
	return nil
}

func (s *Service) configure(ctx context.Context, key string, in ProviderInput) (Endpoint, error) {
	if in.Chain != "" && in.Network != "" && in.Chain != in.Network {
		return Endpoint{}, invalid("chainId and network disagree")
	}
	if in.Network == "" {
		in.Network = in.Chain
	}
	if key == "" && strings.EqualFold(in.Provider, publicNode) && in.URL == nil {
		return s.newPublicNode(ctx, in.Network, in)
	}
	if strings.HasPrefix(key, publicNode+":") && (in.Network != "" || in.Provider != "" || in.URL != nil || in.Bearer != nil) {
		return Endpoint{}, invalid("the default route is fixed; use a separate endpoint for custom settings")
	}
	return s.saveProvider(ctx, key, "", in)
}

func (s *Service) saveProvider(ctx context.Context, key, defaultID string, in ProviderInput) (Endpoint, error) {
	var p Endpoint
	var e error
	newRow := key == ""
	if newRow {
		p.ID = defaultID
		if p.ID == "" {
			p.ID = id()
		}
		p.Enabled = true
	} else {
		p, e = s.endpoint(s.Store.db.QueryRowContext(ctx, "SELECT "+endpointCols+" FROM providers WHERE id=?1", key))
		if errors.Is(e, sql.ErrNoRows) {
			return p, notFound
		}
		if e != nil {
			return p, e
		}
	}
	recheck := newRow || in.URL != nil || in.Bearer != nil || in.Network != "" || in.Chain != "" || in.Provider != ""
	if in.Network != "" {
		p.Network = in.Network
	}
	if in.Provider != "" {
		p.Provider = in.Provider
	}
	if in.URL != nil {
		p.secret.URL = *in.URL
	}
	if in.Bearer != nil {
		p.secret.Bearer = *in.Bearer
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	u, e := url.Parse(p.secret.URL)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return p, invalid("invalid RPC URL")
	}
	if p.Provider == "" || s.cfg.Networks[p.Network].Family == "" {
		return p, invalid("unknown network or missing providerId")
	}
	if recheck {
		p.Height, e = s.height(ctx, p)
		if e != nil {
			return p, invalid("height probe failed")
		}
		if e = s.admit(ctx, p, in); e != nil {
			return p, e
		}
	}
	secret := s.box.Seal(nil, nil, bodyJSON(p.secret), []byte(p.ID))
	if newRow {
		insert := "INSERT INTO providers(id,provider,network,secret,enabled,height) VALUES(?1,?2,?3,?4,?5,?6)"
		if defaultID != "" {
			insert += " ON CONFLICT(id) DO NOTHING"
		}
		_, e = s.Store.db.ExecContext(ctx, insert, p.ID, p.Provider, p.Network, secret, p.Enabled, p.Height)
	} else {
		sql := "UPDATE providers SET provider=?2,network=?3,secret=?4,enabled=?5,height=?6,config_version=config_version+1,generation=generation+1"
		if recheck {
			sql += ",healthy=true,checked=(CAST(unixepoch('subsec')*1000 AS INTEGER)),progressed=(CAST(unixepoch('subsec')*1000 AS INTEGER)),failures=0,level=0,cooldown=NULL,probe_until=NULL,last_error=''"
		}
		r, err := s.Store.db.ExecContext(ctx, sql+" WHERE id=?1 AND config_version=?7", p.ID, p.Provider, p.Network, secret, p.Enabled, p.Height, p.configVersion)
		e = err
		if e == nil && affected(r) == 0 {
			return p, conflict("PROVIDER_CHANGED")
		}
	}
	if e != nil {
		return p, e
	}
	return s.endpoint(s.Store.db.QueryRowContext(ctx, "SELECT "+endpointCols+" FROM providers WHERE id=?1", p.ID))
}

func (s *Service) health(ctx context.Context) {
	ps, e := s.endpoints(ctx, "", false)
	if e != nil {
		return
	}
	type sample struct {
		p   Endpoint
		h   int64
		err error
	}
	ch := make(chan sample, len(ps))
	count := 0
	for _, p := range ps {
		if p.Enabled {
			count++
			go func() { h, e := s.height(ctx, p); ch <- sample{p, h, e} }()
		}
	}
	heads := map[string]int64{}
	all := make([]sample, 0, count)
	for range count {
		v := <-ch
		all = append(all, v)
		if v.err == nil && v.h > heads[v.p.Network] {
			heads[v.p.Network] = v.h
		}
	}
	for _, v := range all {
		h := v.h
		if v.err != nil {
			h = v.p.Height
		}
		limits := s.cfg.Networks[v.p.Network]
		lag, stall := limits.MaxHeightLag, time.Duration(limits.StallSeconds)*time.Second
		if lag == 0 {
			lag = 8
		}
		if stall == 0 {
			stall = 5 * time.Minute
		}
		healthy := v.err == nil && heads[v.p.Network]-h <= lag && (h != v.p.Height || time.Since(v.p.Progressed) < stall)
		_, e = s.Store.db.ExecContext(ctx, `UPDATE providers
            SET healthy=?2, checked=CAST(unixepoch('subsec')*1000 AS INTEGER),
                progressed=CASE WHEN height<>?3 THEN CAST(unixepoch('subsec')*1000 AS INTEGER) ELSE progressed END,
                height=?3 WHERE id=?1 AND config_version=?4 AND checked=?5`, v.p.ID, healthy, h, v.p.configVersion, v.p.Checked.UnixMilli())
		if e != nil {
			return
		}
	}
}

func closeCircuitDelay(level int) time.Duration {
	steps := []int{3, 6, 10, 15, 21, 28, 36, 45, 55, 60}
	return time.Duration(steps[min(max(level, 1), len(steps))-1]) * time.Minute
}
