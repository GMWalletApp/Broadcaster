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

func publicNodeID(network string) string { return publicNode + ":" + network }

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
		if route, ok := s.publicRoute(name); ok {
			result = append(result, defaultProvider{route, publicNode, name})
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
		if n.PublicNodeProbeFile == "" || ctx.Err() != nil {
			continue
		}
		if err := s.admitDefaultFile(ctx, network, n.PublicNodeProbeFile); err != nil && ctx.Err() == nil {
			slog.Error("PublicNode default admission failed", "network", network, "error", err)
		}
	}
}

func (s *Service) admitDefaultFile(ctx context.Context, network, path string) error {
	if _, ok := s.publicRoute(network); !ok {
		return fmt.Errorf("no PublicNode default for network %s", network)
	}
	var exists bool
	if err := s.Store.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM providers WHERE id=?)", publicNodeID(network)).Scan(&exists); err != nil || exists {
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
	_, err = s.configure(ctx, "", ProviderInput{Network: network, Provider: publicNode, EVMProbes: probes.EVMProbes, SolanaProbe: probes.SolanaProbe, NativeProbe: probes.NativeProbe})
	return err
}

func (s *Service) storedDefault(ctx context.Context, network string) (Endpoint, error) {
	return s.endpoint(s.Store.db.QueryRowContext(ctx, "SELECT "+endpointCols+" FROM providers WHERE id=?", publicNodeID(network)))
}

func (s *Service) newPublicNode(ctx context.Context, network string, input ProviderInput) (Endpoint, error) {
	route, ok := s.publicRoute(network)
	if !ok {
		return Endpoint{}, invalid("no PublicNode default for this network")
	}
	if input.MEVProtected != nil {
		return Endpoint{}, invalid("PublicNode protection is set by the built-in catalog")
	}
	if input.Bearer != nil {
		return Endpoint{}, invalid("the public default accepts no bearer credential")
	}
	// Retry after a lost response must not rebroadcast probes or re-enable an
	// endpoint that operations has since disabled.
	existing, err := s.storedDefault(ctx, network)
	if !errors.Is(err, sql.ErrNoRows) {
		return existing, err
	}
	input.URL = &route.URL
	input.Provider = publicNode
	return s.saveProvider(ctx, "", publicNodeID(network), input)
}

func isPublicNodeURL(u *url.URL) bool {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == "publicnode.com" || strings.HasSuffix(host, ".publicnode.com")
}

func (s *Service) identifyRoute(p *Endpoint) {
	u, err := url.Parse(p.secret.URL)
	if err != nil {
		return
	}
	if !isPublicNodeURL(u) {
		p.MEVProtected = p.secret.MEVProtected
		return
	}
	p.Provider = publicNode
	route, ok := s.publicRoute(p.Network)
	p.MEVProtected = ok && route.MEVProtected && strings.TrimSuffix(p.secret.URL, "/") == route.URL
}
