<h1 align="center">Broadcaster</h1>
<p align="center">Manage signed transactions across 18 mainnets and report the outcome to the service that submitted each one.</p>
<p align="center"><strong>English</strong> · <a href="README.zh-CN.md">简体中文</a></p>
<p align="center">Go 1.26.2+ · SQLite · <a href="LICENSE">MIT</a></p>

[Submit a transaction](#submit-an-ethereum-transaction) · [Run locally](#run-locally) · [Networks](#supported-networks) · [API reference (中文)](docs/api.zh-CN.md)

## Why Broadcaster exists

A payment service needs to know what happened to a transfer even if an RPC times out. Once it accepts the signed transaction, Broadcaster saves it in SQLite, sends it to up to three independent providers and tracks inclusion and execution. It resumes outstanding tasks after a restart.

Configure the submitting service’s HTTPS callback URL (`clients[].webhook`) and signing secret (`webhookSecret`). When the task ends, Broadcaster sends a signed `transaction.closed` event back to that service so it can update the order or notify the user. Broadcaster retries failed deliveries. [Callback signing and events (中文)](docs/api.zh-CN.md#故障恢复与上游事件)

## Submit an Ethereum transaction

Start the service and enable an Ethereum provider first. The example uses `curl`, `jq` and your business API token. Set `SIGNED_TX` to a hex-encoded transaction signed for Ethereum mainnet that you intend to broadcast. [Run locally](#run-locally)

```sh
BASE=http://127.0.0.1:8080
TX_HASH=$(jq -n --arg raw "$SIGNED_TX" '{rawTransaction: $raw}' |
  curl -fsS "$BASE/EVM/transactions?chainId=1" \
    -H "Authorization: Bearer $BROADCASTER_API_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @- |
  jq -er '.txHash')
```

HTTP 202 acknowledges that Broadcaster has accepted and saved the task. Tracking continues after an RPC accepts the transaction. A successful completion callback includes the following fields.

```json
{
  "event": "transaction.closed",
  "data": {
    "taskState": "CLOSED",
    "taskOutcome": "INCLUDED",
    "executionStatus": "SUCCESS"
  }
}
```

| Result | Meaning |
|---|---|
| `CONFIRMING`, `executionStatus=PENDING` | Inclusion observed. Broadcaster polls for the execution result. Show the payment as confirming. |
| `CLOSED / INCLUDED`, `executionStatus=SUCCESS` | Inclusion and successful execution verified. |
| `CLOSED / INCLUDED`, `executionStatus=FAILED` | Read `reason` for the cause (for example, out of gas or a contract revert) and `evidence` for available diagnostics. |
| HTTP `409 NONCE_CONFLICT` at intake, or a `REJECTED` delivery | The nonce is already in the local queue, or an individual node refused the transaction. Each refused delivery has its own `reason`, including fee and TRON signature permission errors. |

Deduplicate callbacks by `eventId`. Use the returned transaction hash to inspect progress or reconcile a result. [Failure reasons (中文)](docs/api.zh-CN.md#如何判断失败原因)

```sh
curl -fsS "$BASE/EVM/transactions/$TX_HASH?chainId=1" \
  -H "Authorization: Bearer $BROADCASTER_API_TOKEN" |
  jq '{taskState, taskOutcome, executionStatus}'
```

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

| Mainnets | Submit to | API reference |
|---|---|---|
| Ethereum, Arbitrum One, BNB Smart Chain, Avalanche C-Chain, Polygon PoS, OP Mainnet, Base, HyperEVM, Monad, Arc, Plasma, Robinhood Chain | `POST /EVM/transactions` | [EVM](docs/api.zh-CN.md#evm-调用方接口) |
| Solana | `POST /solana/transactions` | [Solana](docs/api.zh-CN.md#solana-代付交易) |
| Bitcoin, TRON, Aptos, Sui, TON | `POST /networks/{network}/transactions` | [Bitcoin · TRON · Aptos · Sui · TON](docs/api.zh-CN.md#非-evm-调用方接口) |

On EVM, a signer can ask for the next available nonce. If the chain returns 20 and the queue holds 20, 21 and 23, Broadcaster suggests 22. On Solana, the user and fee payer sign the same message. If the blockhash expires, Broadcaster checks the original transaction’s history before requesting new signatures. [EVM nonce guide (中文)](docs/api.zh-CN.md#evm-nonce-查询与任务排序) · [Solana sponsorship (中文)](docs/api.zh-CN.md#solana-代付交易) · [Native transaction formats (中文)](docs/api.zh-CN.md#原生链广播接口)

## Production providers

PublicNode is the only bundled provider. Each network broadcasts through as many healthy independent providers as it has available, up to three per round.

| For three providers per network | What to configure |
|---|---|
| The 15 networks with a PublicNode preset | Enable PublicNode and privately register two additional providers. |
| Monad, Plasma and TON | Register three providers for each network. These networks have no PublicNode mainnet preset. |

Broadcaster prefers MEV-Protected endpoints. If a transaction also goes to an ordinary RPC, that copy can reach the public mempool. [PublicNode routes and protection labels (中文)](docs/api.zh-CN.md#publicnode-默认节点) · [18-network configuration](config.mainnets.json)
