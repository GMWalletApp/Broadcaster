<h1 align="center">Broadcaster</h1>
<p align="center">Send a signed transaction to one of 18 mainnets and find out whether it landed and executed successfully.</p>
<p align="center"><strong>English</strong> · <a href="README.zh-CN.md">简体中文</a></p>
<p align="center">Go 1.26.2+ · SQLite · <a href="LICENSE">MIT</a></p>

[Submit a transaction](#submit-an-ethereum-transaction) · [Run locally](#run-locally) · [Networks](#supported-networks) · [API reference (中文)](docs/api.zh-CN.md)

## Why Broadcaster exists

A wallet submits a transfer and the RPC times out. The node may already have the transaction. The wallet still needs to tell its user what happened to that payment.

Broadcaster saves the signed bytes in SQLite and sends the same transaction to up to three independent RPC providers at once. It keeps checking the chain after a timeout and picks up outstanding tasks after a restart. The private key stays with the signer. The wallet can look up the result by hash or receive a signed webhook.

## Submit an Ethereum transaction

Start the service and enable an Ethereum provider first. The example uses `curl`, `jq` and your business API token. Set `SIGNED_TX` to a hex-encoded transaction signed for Ethereum mainnet that you intend to broadcast. [Run locally](#run-locally)

```sh
BASE=http://127.0.0.1:8080
TX_HASH=$(jq -n --arg raw "$SIGNED_TX" '{rawTransaction: $raw}' |
  curl -fsS "$BASE/transactions?chainId=1" \
    -H "Authorization: Bearer $BROADCASTER_API_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @- |
  jq -er '.txHash')
```

The first submission returns HTTP 202 once Broadcaster has saved the transaction. Use the returned hash to check progress.

```sh
curl -fsS "$BASE/transactions/$TX_HASH?chainId=1" \
  -H "Authorization: Bearer $BROADCASTER_API_TOKEN" |
  jq '{taskState, taskOutcome, executionStatus}'
```

| Result | What the wallet can tell its user |
|---|---|
| `taskState: QUEUED` | The transaction is waiting to be broadcast. |
| `taskOutcome: INCLUDED`, `executionStatus: SUCCESS` | The transaction landed and executed successfully. |
| `taskOutcome: INCLUDED`, `executionStatus: FAILED` | The transaction landed, but execution failed. |
| `taskOutcome: INCLUDED`, `executionStatus: UNKNOWN` | The transaction landed. Check again for its execution result. |

If settlement requires additional confirmations, the application waits for them according to its own policy.

## Run locally

The example configuration starts an Ethereum service. Generate the two tokens and storage key once and save them for restarts.

```sh
go build -o broadcaster ./cmd/broadcaster
cp config.example.json config.json
export BROADCASTER_API_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_ADMIN_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_STORAGE_KEY="$(openssl rand -base64 32)"
./broadcaster -config config.json -db broadcaster.db
```

To enable the bundled Ethereum endpoint, put the signed admission probes in `probes/1.json` before startup, or register the provider later through the admin API. Admission broadcasts those probes on mainnet. [Probe format and provider setup (中文)](docs/api.zh-CN.md#publicnode-默认节点)

## Supported networks

| Mainnets | Submit to |
|---|---|
| Ethereum, Arbitrum One, BNB Smart Chain, Avalanche C-Chain, Polygon PoS, OP Mainnet, Base, HyperEVM, Monad, Arc, Plasma, Robinhood Chain | `POST /transactions` |
| Solana | `POST /solana/transactions` |
| Bitcoin, TRON, Aptos, Sui, TON | `POST /networks/{network}/transactions` |

On EVM, a signer can ask for the next available nonce. If the chain returns 20 and the queue holds 20, 21 and 23, Broadcaster suggests 22. On Solana, the user and fee payer sign the same message. If the blockhash expires, Broadcaster checks the original transaction’s history before requesting new signatures. [EVM nonce guide (中文)](docs/api.zh-CN.md#evm-nonce-查询与任务排序) · [Solana sponsorship (中文)](docs/api.zh-CN.md#solana-代付交易) · [Native transaction formats (中文)](docs/api.zh-CN.md#原生链广播接口)

## Production providers

PublicNode is the only bundled provider. Each network broadcasts through as many healthy independent providers as it has available, up to three per round.

| For three providers per network | What to configure |
|---|---|
| The 15 networks with a PublicNode preset | Enable PublicNode and privately register two additional providers. |
| Monad, Plasma and TON | Register three providers for each network. These networks have no PublicNode mainnet preset. |

Broadcaster prefers MEV-Protected endpoints. If a transaction also goes to an ordinary RPC, that copy can reach the public mempool. [PublicNode routes and protection labels (中文)](docs/api.zh-CN.md#publicnode-默认节点) · [18-network configuration](config.mainnets.json)
