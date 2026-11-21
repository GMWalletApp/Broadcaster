package broadcast

import (
	"context"
	"database/sql"
	"math"
	"math/rand/v2"
	"strconv"
)

type nonceView struct {
	Chain        string `json:"chainId"`
	Address      string `json:"address"`
	Tag          string `json:"blockTag"`
	ChainNonce   string `json:"chainNonce"`
	PendingNonce string `json:"pendingNonce,omitempty"`
	Nonce        string `json:"nonce"`
}

func (s *Service) accountNonce(ctx context.Context, network, address, tag string) (nonceView, error) {
	ps, err := s.endpoints(ctx, network, true)
	if err != nil {
		return nonceView{}, err
	}
	rand.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
	for _, p := range ps {
		if err := ctx.Err(); err != nil {
			return nonceView{}, err
		}
		latest, err := s.nonceAt(ctx, p, address, "latest")
		if err != nil {
			continue
		}
		floor := latest
		view := nonceView{Chain: network, Address: address, Tag: tag, ChainNonce: strconv.FormatUint(latest, 10)}
		if tag == "pending" {
			pending, err := s.nonceAt(ctx, p, address, "pending")
			if err != nil {
				continue
			}
			view.PendingNonce = strconv.FormatUint(pending, 10)
			floor = max(latest, pending)
		}
		next, err := s.Store.nextNonce(ctx, network, address, latest, floor)
		if err != nil {
			return nonceView{}, err
		}
		view.Nonce = strconv.FormatUint(next, 10)
		return view, nil
	}
	return nonceView{}, &Fault{503, "NONCE_VIEW_UNAVAILABLE", ""}
}

// Read the whole account's active slots in one snapshot, including tasks from
// other owners. Only confirmed chain progress can trigger a consumption check.
func (s *Store) nextNonce(ctx context.Context, network, address string, latest, floor uint64) (uint64, error) {
	err := s.atomic(ctx, func(tx *sql.Tx) error {
		confirmed := strconv.FormatUint(latest, 10)
		_, err := tx.ExecContext(ctx, `UPDATE tasks SET state='RESOLVING_NONCE',reason='CONSUMER_UNKNOWN',
			due=0,revision=revision+1,lease=NULL,lease_until=NULL
			WHERE family='evm' AND network=? AND sender=? AND state IN ('QUEUED','TRACKING')
			AND (length(nonce),nonce)<(length(?),?)`, network, address, confirmed, confirmed)
		if err != nil {
			return err
		}
		from := strconv.FormatUint(floor, 10)
		rows, err := tx.QueryContext(ctx, `SELECT nonce FROM tasks
			WHERE family='evm' AND state<>'CLOSED' AND network=? AND sender=?
			AND (length(nonce),nonce)>=(length(?),?) ORDER BY length(nonce),nonce`, network, address, from, from)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var slot string
			if err := rows.Scan(&slot); err != nil {
				return err
			}
			if slot != strconv.FormatUint(floor, 10) || floor == math.MaxUint64 {
				break
			}
			floor++
		}
		return rows.Err()
	})
	if err == nil && floor == math.MaxUint64 {
		return 0, conflict("NONCE_EXHAUSTED")
	}
	return floor, err
}
