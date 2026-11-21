<h1 align="center">Broadcaster</h1>
<p align="center">管理 18 条主网上已签名交易的完整生命周期，将结果回传给提交交易的服务。</p>
<p align="center"><a href="README.md">English</a> · <strong>简体中文</strong></p>
<p align="center">Go 1.26.2+ · SQLite · <a href="LICENSE">MIT</a></p>

[提交一笔交易](#提交一笔以太坊交易) · [部署服务](docs/api.zh-CN.md#运行) · [支持的网络](#支持的网络) · [接口文档](docs/api.zh-CN.md)

## 为什么做 Broadcaster

付款服务提交一笔签好的交易后，即使 RPC 超时，也需要知道这笔付款最终有没有上链。Broadcaster 接手后把签名原文存入 SQLite，向最多三家独立供应商并发广播，并持续核查入块和执行结果。进程重启后会接着处理原任务。

接入时，为提交方配置 HTTPS 回调地址（`clients[].webhook`）和签名密钥（`webhookSecret`）。任务结束时，Broadcaster 会主动把 `transaction.closed` 事件发回该服务。提交方据此更新订单或通知用户；回调投递失败时，Broadcaster 会重试。[回调签名与事件](docs/api.zh-CN.md#故障恢复与上游事件)

## 提交一笔以太坊交易

先按部署指南启动服务并为以太坊启用一个节点。下面的例子使用 `curl` 和 `jq`。将 `SIGNED_TX` 设为钱包已经签好、准备发往以太坊主网的十六进制交易原文，`BROADCASTER_API_TOKEN` 使用部署时设置的业务令牌。[部署服务](docs/api.zh-CN.md#运行) · [启用 PublicNode](docs/api.zh-CN.md#内置主网节点)

```sh
BASE=http://127.0.0.1:8080
TX_HASH=$(jq -n --arg raw "$SIGNED_TX" '{rawTransaction: $raw}' |
  curl -fsS "$BASE/EVM/transactions?chainId=1" \
    -H "Authorization: Bearer $BROADCASTER_API_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @- |
  jq -er '.txHash')
```

首次提交返回 HTTP 202，表示 Broadcaster 已受理并保存任务。节点接收交易后，Broadcaster 继续跟踪。成功路径的结束回调包含以下字段（节选）。

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

| 提交方看到的结果 | 含义 |
|---|---|
| `CONFIRMING`，`executionStatus=PENDING` | 已看到入块，正在等执行回执。Broadcaster 自动复查，付款继续显示「确认中」。 |
| `CLOSED / INCLUDED`，`executionStatus=SUCCESS` | 已核实入块和执行成功。 |
| `CLOSED / INCLUDED`，`executionStatus=FAILED` | 链上执行失败。读 `reason` 区分 Gas 耗尽、合约回退等原因，`evidence` 提供节点可用的诊断信息。 |
| 提交返回 `409 NONCE_CONFLICT`，或某节点广播记录为 `REJECTED` | 前者表示本地 Nonce 已占用。后者查看该节点的 `reason`，例如费用过低、TRON 多签权重不足或签名账户无权限。 |

回调接收方按 `eventId` 去重。排查进度或补查结果时，可用提交时返回的哈希查询。[失败原因和回调字段](docs/api.zh-CN.md#如何判断失败原因)

```sh
curl -fsS "$BASE/EVM/transactions/$TX_HASH?chainId=1" \
  -H "Authorization: Bearer $BROADCASTER_API_TOKEN" |
  jq '{taskState, taskOutcome, executionStatus}'
```

如果付款需要等待多个确认，业务方按自己的结算规则继续等待。[完整状态机](docs/api.zh-CN.md#evm-交易生命周期)

## 支持的网络

| 主网 | 提交入口 | 接口文档 |
|---|---|---|
| Ethereum、Arbitrum One、BNB Smart Chain、Avalanche C-Chain、Polygon PoS、OP Mainnet、Base、HyperEVM、Monad、Arc、Plasma、Robinhood Chain | `POST /EVM/transactions` | [EVM](docs/api.zh-CN.md#evm-调用方接口) |
| Solana | `POST /solana/transactions` | [Solana](docs/api.zh-CN.md#solana-代付交易) |
| Bitcoin、TRON、Aptos、Sui、TON | `POST /networks/{network}/transactions` | [Bitcoin · TRON · Aptos · Sui · TON](docs/api.zh-CN.md#非-evm-调用方接口) |

EVM 签名前可以查询推荐 nonce。比如链上返回 20，队列已占用 20、21、23，服务会推荐 22。Solana 代付时，用户和代付方先签署同一份消息；如果 blockhash 过期，服务会核查原交易是否入块，再决定是否要求重签。[EVM nonce 与替换](docs/api.zh-CN.md#evm-nonce-查询与任务排序) · [Solana 代付](docs/api.zh-CN.md#solana-代付交易) · [其他五条链的签名格式](docs/api.zh-CN.md#原生链广播接口)

## 生产环境怎么配节点

18 条主网各内置一家供应商。通过准入后，每轮向最多三家健康供应商并发广播。部署时可私下登记额外的付费节点。

| 主网 | 内置供应商与地址 |
|---|---|
| 其余 15 条主网 | PublicNode |
| Monad | Monad Foundation · `https://rpc-mainnet.monadinfra.com` |
| Plasma | Plasma 官方 RPC · `https://rpc.plasma.to` |
| TON | TON Center · `https://toncenter.com`，v2 广播、v3 查询 |

这三个公开端点均有限流。Monad Foundation 将其公开端点列为开发用途，生产部署请选用承载生产流量的供应商。[Monad 主网说明](https://docs.monad.xyz/developer-essentials/network-information)

调度器优先使用标有 MEV-Protected 的端点。如果同一笔交易也发给普通 RPC，该副本仍可能进入公开交易池。[PublicNode 端点和保护标记](docs/api.zh-CN.md#内置主网节点) · [18 条主网配置](config.mainnets.json)
