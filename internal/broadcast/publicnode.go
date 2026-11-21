package broadcast

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
)

const publicNode = "publicnode"
const solanaMainnetGenesis = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"

type publicRoute struct {
	URL          string `json:"url"`
	MEVProtected bool   `json:"mevProtected"`
}

// A positive flag refers to the exact HTTPS RPC route labeled by PublicNode.
// Keep the reviewed URLs local so a change to the website cannot redirect sends.
var publicEVM = map[string]publicRoute{
	"1":     {"https://ethereum-rpc.publicnode.com", true},
	"10":    {"https://optimism-rpc.publicnode.com", false},
	"56":    {"https://bsc-rpc.publicnode.com", true},
	"137":   {"https://polygon-bor-rpc.publicnode.com", false},
	"999":   {"https://hyperliquid-rpc.publicnode.com", false},
	"4663":  {"https://robinhood-rpc.publicnode.com", true},
	"5042":  {"https://arc-rpc.publicnode.com", true},
	"8453":  {"https://base-rpc.publicnode.com", true},
	"42161": {"https://arbitrum-one-rpc.publicnode.com", true},
	"43114": {"https://avalanche-c-chain-rpc.publicnode.com", false},
}

func (s *Service) publicRoute(network string) (publicRoute, bool) {
	n := s.cfg.Networks[network]
	switch n.Family {
	case EVM:
		r, ok := publicEVM[network]
		return r, ok
	case Solana:
		if n.GenesisHash == solanaMainnetGenesis {
			return publicRoute{"https://solana-rpc.publicnode.com", true}, true
		}
	case Bitcoin:
		return publicRoute{"https://bitcoin-rpc.publicnode.com", false}, true
	case Tron:
		return publicRoute{"https://tron-rpc.publicnode.com", false}, true
	case Sui:
		return publicRoute{"https://sui-grpc.publicnode.com:443", false}, true
	case Aptos:
		return publicRoute{"https://aptos-rest.publicnode.com/v1", false}, true
	}
	return publicRoute{}, false
}

var officialDefaults = map[string]defaultProvider{
	"143":  {publicRoute{"https://rpc-mainnet.monadinfra.com", false}, "monad-foundation", "143"},
	"9745": {publicRoute{"https://rpc.plasma.to", false}, "plasma", "9745"},
	"ton":  {publicRoute{"https://toncenter.com", false}, "toncenter", "ton"},
}

func (s *Service) preset(network string) (defaultProvider, bool) {
	if route, ok := s.publicRoute(network); ok {
		return defaultProvider{route, publicNode, network}, true
	}
	preset, ok := officialDefaults[network]
	family := s.cfg.Networks[network].Family
	return preset, ok && (network == "ton" && family == TON || network != "ton" && family == EVM)
}

func (p defaultProvider) id() string { return p.Provider + ":" + p.Network }

type defaultProvider struct {
	publicRoute
	Provider string `json:"providerId"`
	Network  string `json:"network"`
}

// Presets are visible before admission; workers only select persisted providers.
func (s *Service) defaultProviders(network string) []defaultProvider {
	result := []defaultProvider{}
	for _, name := range slices.Sorted(maps.Keys(s.cfg.Networks)) {
		if network != "" && name != network {
			continue
		}
		if route, ok := s.preset(name); ok {
			result = append(result, route)
		}
	}
	return result
}

type admissionProbes struct {
	NativeProbe *NativeRequest `json:"nativeProbe"`
	EVMProbes   []string       `json:"evmProbes"`
	SolanaProbe *SolanaRequest `json:"solanaProbe"`
}

func (s *Service) admitConfiguredDefaults(ctx context.Context) {
	for network, n := range s.cfg.Networks {
		if n.DefaultProbeFile == "" || ctx.Err() != nil {
			continue
		}
		if err := s.admitDefaultFile(ctx, network, n.DefaultProbeFile); err != nil && ctx.Err() == nil {
			slog.Error("default provider admission failed", "network", network, "error", err)
		}
	}
}

func (s *Service) admitDefaultFile(ctx context.Context, network, path string) error {
	preset, ok := s.preset(network)
	if !ok {
		return fmt.Errorf("no default provider for network %s", network)
	}
	var exists bool
	if err := s.Store.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM providers WHERE id=?)", preset.id()).Scan(&exists); err != nil || exists {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	const maxSize = 4 << 20
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSize {
		return errors.New("expected a probe JSON file of at most 4 MiB")
	}
	var probes admissionProbes
	decoder := json.NewDecoder(io.LimitReader(f, maxSize+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&probes); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("unexpected trailing probe data")
	}
	_, err = s.configure(ctx, "", ProviderInput{Network: network, Provider: preset.Provider, EVMProbes: probes.EVMProbes, SolanaProbe: probes.SolanaProbe, NativeProbe: probes.NativeProbe})
	return err
}

func (s *Service) storedDefault(ctx context.Context, network string) (Endpoint, error) {
	preset, ok := s.preset(network)
	if !ok {
		return Endpoint{}, sql.ErrNoRows
	}
	return s.endpoint(s.Store.db.QueryRowContext(ctx, "SELECT "+endpointCols+" FROM providers WHERE id=?", preset.id()))
}

func (s *Service) newDefault(ctx context.Context, preset defaultProvider, input ProviderInput) (Endpoint, error) {
	if input.MEVProtected != nil {
		return Endpoint{}, invalid("default route protection is set by the built-in catalog")
	}
	if input.Bearer != nil {
		return Endpoint{}, invalid("the public default accepts no bearer credential")
	}
	// Retry after a lost response must not rebroadcast probes or re-enable an
	// endpoint that operations has since disabled.
	existing, err := s.storedDefault(ctx, preset.Network)
	if !errors.Is(err, sql.ErrNoRows) {
		return existing, err
	}
	input.URL = &preset.URL
	input.Provider = preset.Provider
	return s.saveProvider(ctx, "", preset.id(), input)
}

func isPublicNodeURL(u *url.URL) bool {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == "publicnode.com" || strings.HasSuffix(host, ".publicnode.com")
}

// Match operator-owned hosts even when an administrator registers an alias.
func catalogOperator(u *url.URL) string {
	if isPublicNodeURL(u) {
		return publicNode
	}
	for _, p := range officialDefaults {
		if strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), strings.TrimPrefix(p.URL, "https://")) {
			return p.Provider
		}
	}
	return ""
}

func (s *Service) identifyRoute(p *Endpoint) {
	u, err := url.Parse(p.secret.URL)
	if err != nil {
		return
	}
	operator := catalogOperator(u)
	if operator == "" {
		p.MEVProtected = p.secret.MEVProtected
		return
	}
	p.Provider = operator
	route, ok := s.publicRoute(p.Network)
	p.MEVProtected = ok && route.MEVProtected && strings.TrimSuffix(p.secret.URL, "/") == route.URL
}
