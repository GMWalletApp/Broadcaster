package broadcast

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type handler func(http.ResponseWriter, *http.Request, Client) error

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	bind := func(pattern string, admin bool, h handler) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
			w.Header().Set("Cache-Control", "no-store")
			auth := r.Header.Get("Authorization")
			for _, c := range s.cfg.Clients {
				if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+c.Token)) != 1 {
					continue
				}
				if admin && !c.Admin {
					respondError(w, &Fault{403, "FORBIDDEN", ""})
					return
				}
				if e := h(w, r, c); e != nil {
					respondError(w, e)
				}
				return
			}
			respondError(w, &Fault{401, "UNAUTHORIZED", ""})
		})
	}
	bind("POST /networks/{network}/transactions", false, s.nativeTransactions)
	bind("GET /networks/{network}/transactions/{hash}", false, s.nativeTransactions)
	// Older clients use the same handlers without a namespace.
	for _, prefix := range []string{"/EVM", ""} {
		bind("POST "+prefix+"/transactions", false, s.evmSubmit)
		bind("POST "+prefix+"/transactions/{hash}/replacement", false, s.evmSubmit)
		bind("GET "+prefix+"/transactions/{hash}", false, s.getTask(EVM))
		bind("POST "+prefix+"/transactions/{hash}/force", false, s.force)
		bind("GET "+prefix+"/accounts/{address}/queue", false, s.queue)
		bind("GET "+prefix+"/accounts/{address}/nonce", false, s.nonce)
	}
	bind("POST /solana/transactions", false, s.solSubmit)
	bind("POST /solana/transactions/{signature}/resign", false, s.solSubmit)
	bind("GET /solana/transactions/{signature}", false, s.getTask(Solana))
	bind("POST /rpc-providers", true, s.putProvider)
	bind("PATCH /rpc-providers/{id}", true, s.putProvider)
	bind("GET /rpc-providers/defaults", true, func(w http.ResponseWriter, r *http.Request, c Client) error {
		writeJSON(w, 200, s.defaultProviders(r.URL.Query().Get("network")))
		return nil
	})
	bind("GET /rpc-providers", true, func(w http.ResponseWriter, r *http.Request, c Client) error {
		ps, e := s.endpoints(r.Context(), r.URL.Query().Get("chainId"), false)
		if e == nil {
			writeJSON(w, 200, ps)
		}
		return e
	})
	return mux
}

func parse(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return invalid("invalid JSON body: " + e.Error())
	}
	if d.Decode(new(any)) != io.EOF {
		return invalid("expected one JSON object")
	}
	return nil
}

func receipt(w http.ResponseWriter, t Task, hash string, created bool) {
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	key := "txHash"
	if t.Family == Solana {
		key = "signature"
	}
	writeJSON(w, status, map[string]any{"taskId": t.ID, key: hash, "state": t.State, "network": t.Network})
}

func (s *Service) evmSubmit(w http.ResponseWriter, r *http.Request, c Client) error {
	var in struct {
		Raw string `json:"rawTransaction"`
	}
	if e := parse(r, &in); e != nil {
		return e
	}
	t, v, e := decodeEVM(in.Raw)
	if e != nil {
		return e
	}
	if s.cfg.Networks[t.Network].Family != EVM {
		return invalid("unsupported chain")
	}
	if n := r.URL.Query().Get("chainId"); n != "" && n != t.Network {
		return invalid("chain mismatch")
	}
	t.Owner = c.Name
	old := strings.ToLower(r.PathValue("hash"))
	var created bool
	if old != "" {
		t, created, e = s.Store.replace(r.Context(), c.Name, old, t, v)
	} else {
		// Local idempotency/conflict takes precedence over a stale chain nonce.
		existing, err := s.Store.submittedEVM(r.Context(), t, v.Hash)
		switch {
		case err == nil:
			receipt(w, existing, v.Hash, false)
			return nil
		case err != notFound:
			return err
		}
		// When no providers are available, intake still durably queues the task.
		ps, err := s.endpoints(r.Context(), t.Network, true)
		if err != nil {
			return err
		}
		for _, p := range ps {
			n, err := s.nonceAt(r.Context(), p, t.Sender, "latest")
			if err != nil {
				continue
			}
			nonce, _ := strconv.ParseUint(t.Nonce, 10, 64)
			if n > nonce {
				_, found, err := s.evmLocation(r.Context(), p, &v)
				if err != nil {
					continue
				}
				if !found {
					head, err := s.block(r.Context(), p, "latest", false)
					if err == nil {
						height, err := hexutil.DecodeUint64(head.Height)
						if err == nil {
							consumer, block, err := s.consumer(r.Context(), p, t, nonce, height)
							if err == nil && consumer != "" && consumer != v.Hash && s.agreeOnBlock(r.Context(), ps, p.ID, block.Height, block.Hash) {
								return conflict("NONCE_CONSUMED")
							}
						}
					}
				}
			}
			if len(v.Authorizations) > 0 {
				if b, err := s.block(r.Context(), p, "latest", false); err == nil {
					s.diagnose(r.Context(), p, &v, t.Sender, b.Height, false)
				}
			}
			break
		}
		t, created, e = s.Store.submit(r.Context(), t, v)
	}
	if e == nil {
		receipt(w, t, v.Hash, created)
	}
	return e
}

func (s *Service) solSubmit(w http.ResponseWriter, r *http.Request, c Client) error {
	var in SolanaRequest
	if e := parse(r, &in); e != nil {
		return e
	}
	n := s.cfg.Networks[in.Cluster]
	if n.Family != Solana {
		return invalid("unknown cluster")
	}
	t, v, e := decodeSolana(in, n)
	if e != nil {
		return e
	}
	t.Owner = c.Name
	old := r.PathValue("signature")
	// A repeat request after expiry still returns the original task.
	existing, lookupErr := s.Store.lookup(r.Context(), c.Name, Solana, t.Network, v.Hash)
	if lookupErr == nil && old == "" {
		if existing.Payment != in.Payment {
			return conflict("PAYMENT_CONFLICT")
		}
		receipt(w, existing, v.Hash, false)
		return nil
	}
	if lookupErr != nil && lookupErr != notFound {
		return lookupErr
	}
	if lookupErr == notFound {
		if err := s.checkWindow(r.Context(), &v, t.Network); err != nil {
			return err
		}
	}
	var created bool
	if old != "" {
		t, created, e = s.Store.replace(r.Context(), c.Name, old, t, v)
	} else {
		if strings.TrimSpace(in.Payment) == "" || len(in.Payment) > 256 {
			return invalid("paymentId required (up to 256 bytes)")
		}
		t, created, e = s.Store.submit(r.Context(), t, v)
	}
	if e == nil {
		receipt(w, t, v.Hash, created)
	}
	return e
}

func (s *Service) getTask(f Family) handler {
	return func(w http.ResponseWriter, r *http.Request, c Client) error {
		network := r.URL.Query().Get("chainId")
		hash := strings.ToLower(r.PathValue("hash"))
		if f == Solana {
			network = r.URL.Query().Get("cluster")
			hash = r.PathValue("signature")
		}
		t, e := s.Store.lookup(r.Context(), c.Name, f, network, hash)
		if e != nil {
			return e
		}
		if t.Outcome == "INCLUDED" && t.Execution == "UNKNOWN" {
			s.completeReceipt(r.Context(), &t)
		}
		writeJSON(w, 200, struct {
			Task
			Queried string `json:"queriedHash"`
		}{t, hash})
		return nil
	}
}

func (s *Service) force(w http.ResponseWriter, r *http.Request, c Client) error {
	var in struct {
		Reason string `json:"reason"`
	}
	if e := parse(r, &in); e != nil {
		return e
	}
	if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 512 {
		return invalid("reason required (up to 512 bytes)")
	}
	var t Task
	hash := strings.ToLower(r.PathValue("hash"))
	e := s.Store.atomic(r.Context(), func(tx *sql.Tx) error {
		var err error
		t, err = find(r.Context(), tx, c.Name, EVM, r.URL.Query().Get("chainId"), hash)
		if err != nil {
			return err
		}
		if t.Active != hash || t.State == "CLOSED" || t.State == "RESOLVING_NONCE" || t.State == "CONFIRMING" {
			return conflict("NOT_SCHEDULABLE")
		}
		_, err = tx.ExecContext(r.Context(), "INSERT INTO force_audit(task,actor,reason) VALUES(?,?,?)", t.ID, c.Name, in.Reason)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(r.Context(), "UPDATE tasks SET forced=1,due=0 WHERE id=?", t.ID)
		return err
	})
	if e == nil {
		receipt(w, t, hash, true)
	}
	return e
}

func (s *Service) account(r *http.Request) (string, string, error) {
	address := r.PathValue("address")
	network := r.URL.Query().Get("chainId")
	if s.cfg.Networks[network].Family != EVM || !common.IsHexAddress(address) {
		return "", "", invalid("valid EVM address and configured chainId required")
	}
	return strings.ToLower(address), network, nil
}

func (s *Service) nonce(w http.ResponseWriter, r *http.Request, c Client) error {
	address, n, e := s.account(r)
	if e != nil {
		return e
	}
	tag := r.URL.Query().Get("blockTag")
	if tag == "" {
		tag = "latest"
	}
	if tag != "latest" && tag != "pending" {
		return invalid("blockTag must be latest or pending")
	}
	view, e := s.accountNonce(r.Context(), n, address, tag)
	if e != nil {
		return e
	}
	writeJSON(w, 200, view)
	return nil
}

func (s *Service) queue(w http.ResponseWriter, r *http.Request, c Client) error {
	address, n, e := s.account(r)
	if e != nil {
		return e
	}
	rows, e := s.Store.db.QueryContext(r.Context(), "SELECT "+taskColumns+" FROM tasks WHERE owner_id=? AND network=? AND sender=? AND state<>'CLOSED' ORDER BY length(nonce),nonce", c.Name, n, address)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, e := scanTask(rows)
		if e != nil {
			return e
		}
		out = append(out, t)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Service) putProvider(w http.ResponseWriter, r *http.Request, c Client) error {
	var in ProviderInput
	if e := parse(r, &in); e != nil {
		return e
	}
	p, e := s.configure(r.Context(), r.PathValue("id"), in)
	if e == nil {
		status := 200
		if r.Method == "POST" {
			status = 201
		}
		writeJSON(w, status, p)
	}
	return e
}

func (s *Service) Serve(ctx context.Context) error {
	srv := http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, IdleTimeout: time.Minute}
	run, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() { s.Run(run); close(done) }()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-run.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			_ = srv.Close()
		}
	}()
	e := srv.ListenAndServe()
	stop()
	<-done
	<-stopped
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
