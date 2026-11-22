# Broadcaster 接口与交易生命周期

[中文首页](../README.zh-CN.md) · [English](api.md) · [主网配置](../config.mainnets.json)

[运行](#运行) · [EVM 接口](#evm-调用方接口) · [非 EVM 接口](#非-evm-调用方接口) · [生命周期](#evm-交易生命周期) · [回调与恢复](#故障恢复与上游事件) · [节点管理](#rpc-节点管理) · [验证与维护](#验证与维护)

## 运行

按[快速开始](../README.zh-CN.md#快速开始)构建并启动服务。默认监听 `127.0.0.1:8080`，对外提供服务时可在反向代理终止 TLS。

| 配置项 | 用途 |
|---|---|
| `-config`、`-db` | 指定私有配置和本机 SQLite 文件。示例配置启用以太坊，主网配置包含 18 条链。 |
| `BROADCASTER_STORAGE_KEY` | Base64 编码的 32 字节密钥，用于加密节点凭证。与数据库一起备份，重启时沿用原值。 |
| `clients[].name`、`token`、`admin` | 每个服务使用独立名称和至少 24 字节的 Bearer Token。管理令牌还可操作节点。 |
| `clients[].webhook`、`webhookSecret` | 回调接收地址使用 HTTPS，独立签名密钥至少 24 字节。令牌与签名密钥支持从环境变量读取。 |
| `networks` | EVM 使用十进制 chain ID，Solana 使用集群名，其余链使用网络名。Solana 还须配置 genesis hash 和允许代付的公钥。 |
| `workers`、`trackingHours` | 默认 4 个工作实例。EVM 和五条原生链默认在未见入块时跟踪 24 小时。Solana 按 blockhash 有效窗口与最终链历史判断。 |
| `defaultProbeFile` | 内置节点的准入探针，路径相对于配置文件。兼容旧键 `publicNodeProbeFile`。 |

配置一个可接收回调的业务客户端（部署前设置两个环境变量）：

```json
{"name":"wallet","token":"${BROADCASTER_API_TOKEN}","webhook":"https://payments.example.com/broadcaster","webhookSecret":"${BROADCASTER_WEBHOOK_SECRET}"}
```

请求携带 `Authorization: Bearer <token>`，JSON 请求体使用 `Content-Type: application/json`。错误响应包含 `error` 和可选的 `detail`。SQLite 启用 WAL、外键和 FULL 同步。运行中使用 SQLite 在线备份；停机复制前完成 WAL 检查点。

## EVM 调用方接口

本节所有接口使用大写 `/EVM` 前缀。旧客户端可继续使用原有的无前缀路径。交易提交和管理操作需要鉴权，查询和变更限定在调用方有权访问的任务或账户内。POST 和 PATCH 的请求体使用 JSON。下表中的 rawTransaction 是完整的十六进制已签名原文。Hash 路径支持用 chainId 查询参数限定网络；出现跨链歧义时必须指定链。

| 功能 | 方法与路径 | 输入 | 响应与状态变化 |
|---|---|---|---|
| 提交已签名交易并开始跟踪 | `POST /EVM/transactions` | rawTransaction | 解析签名、链、发送方、外层 Nonce 和本地 Hash。首次持久化返回 202 及 taskId、txHash、state。重复提交相同 Hash 返回 200 及已有任务。同链、同发送方、同 Nonce 已由其他活动任务占用时返回 409 NONCE_CONFLICT。 |
| 按哈希查询进度与执行结果 | `GET /EVM/transactions/{hash}` | 原始或替换 Hash | 返回 200，包含任务阶段和结果、所查 Hash 的状态、当前调度版本、全部候选、各轮各节点结果、链上证据及 7702 诊断。未知或不可见的 Hash 返回 404。 |
| 跳过本地 Nonce 排序，立即安排广播 | `POST /EVM/transactions/{hash}/force` | reason | 对当前可调度版本立即安排一轮广播，记录操作者、时间和原因，返回 202。只跳过本地 Nonce 排序，仍受节点冷却、可用性及链上状态约束。同一轮已排队或在途时复用该轮。 |
| 用同一 Nonce 的新签名交易替换当前版本 | `POST /EVM/transactions/{hash}/replacement` | rawTransaction | 用路径指定待替换的当前版本。新签名交易通过校验，且 chainId、sender、nonce 相同后，在原任务下登记新 Hash，返回 202。重复提交同一已登记替换版本返回 200。原 Hash 继续接受链上监控。 |
| 查看账户中等待处理的交易 | `GET /EVM/accounts/{address}/queue?chainId=…` | 地址、目标链 | 按外层 Nonce 升序返回未关闭任务、当前版本、排队原因和下次调度时间。队列只列已接收的任务。 |
| 查询签名时可用的下一个 Nonce | `GET /EVM/accounts/{address}/nonce?chainId=…` | 地址、目标链，可选 blockTag（latest 或 pending） | 返回最新链上 chainNonce 及结合队列的推荐 nonce。指定 pending 时还返回同一节点的 pendingNonce。均为十进制字符串。所有节点读取失败返回 503 NONCE_VIEW_UNAVAILABLE。 |

**冲突与错误。** 原文无法解码、签名无效、链未启用或整笔交易违反协议规则时，提交返回 4xx 和具体原因，校验失败的请求不占用 Nonce。已有 Hash 的幂等查询优先于依赖当前链状态的预检，先校验调用方访问权限。活动 EVM 任务的唯一键为 (chainId, sender, nonce)，签名交易以 (chainId, txHash) 去重。Hash 幂等检查与新任务插入在同一事务内完成，唯一键冲突时重新读取已提交的任务作判定。并发 replacement 使用当前版本的条件更新；对过时版本提交不同的新 Hash、在核查 Nonce 消耗者或已关闭时尝试替换，以及对不可调度版本执行 force，均返回 409 和原因。已关闭任务保留提交历史及终止结果。

**提交响应示例**（标识符省略中间字符）：

~~~json
{"taskId":"task_42","network":"1","txHash":"0x…","state":"QUEUED"}
~~~

202 表示服务已持久化任务。调用方可用返回的 Hash 查询进度；自动重播、查回执和故障恢复无需调用 retry 接口。

### EVM Nonce 查询与任务排序

每次 `GET /EVM/accounts/{address}/nonce?chainId=…` 随机选择一个健康 RPC 读取 `eth_getTransactionCount(address,"latest")`，失败后换下一家。接口从链上值开始，跳过同链同发送方所有调用方的连续活动任务，返回第一个空位。可选 `blockTag=pending` 会在同一节点再读取 pending，从两者较大值开始。只有 latest 能触发已占用 Nonce 的链上核查。[Ethereum JSON-RPC](https://ethereum.org/developers/docs/apis/json-rpc/#eth_gettransactioncount)

例如链上返回 20，本地活动任务占用 20、21、23，默认响应含 `"chainNonce":"20","nonce":"22"`。签名交易成功入队时才占用槽位；查询本身不预留，也不保存链上读数或推荐值。7702 代付时对代付地址调用此接口，为外层签名选择 Nonce；用户授权使用用户自己的账户 Nonce。

~~~mermaid
flowchart TD
    Read["GET nonce：随机 RPC 查 latest，再查活动队列"] --> Sign["选取外层发送账户的 Nonce，签名"]
    Sign --> Submit["POST /EVM/transactions：原子登记任务"]
    Submit -->|相同 Hash| Existing["返回已有任务"]
    Submit -->|不同 Hash 且任务键冲突| Conflict["409 NONCE_CONFLICT，本次提交不入队"]
    Submit -->|新任务| Queue["按链、外层发送方和 Nonce 排队"]
    Conflict -->|独立付款| Read
    Conflict -->|确需替换原任务| Replace["有权限的调用方显式提交 replacement"]
    Replace --> Queue
~~~

数据库保存签名原文及任务键 (chainId, sender, nonce)，用于并发判重、排序和重启恢复。若两个服务都在第一笔交易入队前查询，它们可能拿到同一个推荐值。不同交易使用同一槽位时，只有一次普通提交能创建任务。另一笔若是独立付款，由签名方再次查询推荐值后重新签名。只有同一任务的有意替换才用 replacement。

发现外层账户的链上 Nonce 已越过本地任务时，逐笔核查候选 Hash。自己的版本入块则核查执行结果后关闭；证实外部消耗则通知原提交方并重新调度剩余任务；证据不足则暂停该任务重播并继续核查。查询发现低于 latest 的排队或跟踪任务，会在同一数据库事务内转入 RESOLVING_NONCE 并唤醒工作实例。工作实例核实结果后通知上游。已关闭任务保留 Hash 历史；若链上槽位仍可用，它可以被新任务使用。过期任务的原文仍可能入块，重付前须核对业务结果。

### ERC-4337 广播与查询

| 调用方 → 服务 | 接口与标识 | 职责 |
|---|---|---|
| 钱包 → Bundler | eth_supportedEntryPoints、eth_sendUserOperation；返回 userOpHash | 钱包选择该链上 Bundler 支持的 EntryPoint，提交签名的 UserOperation。Bundler 校验、模拟并打包。 |
| Bundler → Broadcaster | POST /EVM/transactions，传外层 rawTransaction；返回 taskId、txHash | Bundler 可先查询**外层签名账户**的 Nonce，再签署调用 EntryPoint.handleOps 的交易。沿用同一套最多三家并发、自动重播、显式替换和核实执行结果后关闭的规则。携带新 7702 授权的 bundle 使用 Type 4 并执行逐条授权诊断。 |
| Bundler → Broadcaster | GET /EVM/transactions/{hash} 或入块通知 | Bundler 保存 userOpHash 到外层任务的关联，按 winningHash 定位实际入块版本。若发生替换或重新打包，以实际入块交易中可核实的操作为准。 |
| 钱包 → Bundler | eth_getUserOperationReceipt(userOpHash) | 按 EntryPoint 事件核实指定操作是否入块，通过单条操作回执的 success 报告执行结果。一个 bundle 内各操作可能有不同结果。 |

在 ERC-4337 路径中，Bundler 自行选定外层发送账户的交易 Nonce；随包提供新 7702 授权时，授权账户的签名规则见下节。智能账户的 UserOperation Nonce 由对应 EntryPoint 按账户和 nonce key 管理，签名前可调用 EntryPoint.getNonce(sender, key)。外层交易 CLOSED / INCLUDED 后，Bundler 仍须按 userOpHash 核实每笔操作；外层回执成功不能证明每笔操作成功。[ERC-4337](https://eips.ethereum.org/EIPS/eip-4337) · [ERC-7769 Bundler RPC](https://eips.ethereum.org/EIPS/eip-7769)

### EIP-7702 委托与后续 Gas 代付

设 A 是用户地址，R 是 GM 专用代付地址，D 是 GM 的委托实现合约。以下描述 R 与 A 不同、GM 直接代付的路径。

| 场景 | 用户 A 签什么 | 谁用哪个 Nonce |
|---|---|---|
| 首次设置或变更委托 | A 签署指向 D 的 7702 授权，R 将授权装入 Type 4 交易并签署外层交易。 | 授权使用 **A 的链上账户 Nonce**，外层交易使用 **R 的链上账户 Nonce**。A 自己从外部钱包发出的普通交易也消耗 A 的账户 Nonce。 |
| 委托已生效，后续代付转账 | A 按 D 的规则签署本次转账指令，绑定目标、调用数据、金额及防重放凭据。R 签署发往 **A** 的外层交易并支付 Gas，链在 A 的账户上下文执行 D 的代码。 | 外层仍使用 **R 的链上账户 Nonce**。指令使用 D 为 A 定义的防重放机制；若采用顺序操作 Nonce，同一用户的并发签名须协调这个编号。沿用现有委托时无需再次签署 7702 授权。 |

**首次授权。** 签名方查询 A 的账户 Nonce 来签署授权，查询 R 的账户 Nonce 来签署外层交易。Broadcaster 只用 R 的外层 Nonce 作为任务排序和冲突键，从签名原文中的授权列表诊断 A 的授权。若 A 的外部转账抢先入块，通知签名方核查授权；已经发出的 R 交易继续按其 Hash 跟踪。

**后续代付的调用顺序。**

1. 代付服务查询 R 的账户 Nonce，并根据自己的在途交易选值。A 为本次操作签署 D 要求的指令。
2. 代付服务构造并签署 R 的外层交易，保存业务请求与已签名原文、Hash 的关联，调用 POST /EVM/transactions。Broadcaster 在 R 的队列中按外层 Nonce 调度。
3. 提交响应超时则重交相同原文或查询原 Hash。另一笔付款误用相同 Nonce 时，普通提交返回 409；只有需要替换已接收任务时才提交同 Nonce 的签名替换版本。

例如，代付签名方为 R 的两笔付款选定 `100` 和 `101`。如果两笔都属于 A，而 D 对 A 采用顺序操作编号，签名流程还要分别协调 `7` 和 `8`，并保持执行顺序。A 用外部钱包普通转账只会推进 A 的链上账户 Nonce；已签操作仍可能因余额变化而执行失败。实际操作编号与签名字段以所部署的 D 为准。[EIP-7702 授权处理](https://eips.ethereum.org/EIPS/eip-7702#behavior) · [委托持久性](https://eips.ethereum.org/EIPS/eip-7702#persistence-of-code-delegation) · [签名与防重放](https://eips.ethereum.org/EIPS/eip-7702#security-considerations)

### EIP-7702 诊断

1. **提交时**区分整笔无效和个别授权可能被跳过。空授权列表等整笔格式错误直接返回；逐条预检保留索引、委托目标、可恢复的授权者地址、签名 Nonce、原因及所用区块。按协议顺序计算外层发送者 Nonce 的自增，以及同一列表里前序有效授权的影响。
2. **执行时**分别报告 RPC 广播、外层回执和每条授权结果。使用交易在区块中的实际位置重放或等价证据，才能确定该条 APPLIED 或 SKIPPED 及原因。只有预检、标准回执或区块末账户代码时，对无法证明的条目返回 UNKNOWN，并保留预测及证据来源。
3. **记录结果时**绑定观测到的区块 Hash。外层 status=1 时部分授权仍可能被跳过；外层执行回退时已应用的委托仍可保留。[EIP-7702 协议](https://eips.ethereum.org/EIPS/eip-7702#behavior)

## 非 EVM 调用方接口

使用业务 Bearer Token，以 JSON 提交签名原文。首次受理返回 HTTP 202 和 `taskId`。Solana 返回 `signature`，其余五条链返回 `txHash`。查询返回 HTTP 200 和任务状态，重复提交同一笔已登记交易返回原任务。

| 主网 | 提交 | 查询 | 请求体与查询标识 |
|---|---|---|---|
| Solana | `POST /solana/transactions` | `GET /solana/transactions/{signature}?cluster=solana` | 传 `cluster`、`paymentId`、全签的 Base64 `signedTransaction`、`context.slot` 和 `lastValidBlockHeight`。用第一份签名查询。 |
| Bitcoin | `POST /networks/bitcoin/transactions` | `GET /networks/bitcoin/transactions/{txHash}` | `rawTransaction` 为十六进制 wire 交易，用 txid 查询。 |
| TRON | `POST /networks/tron/transactions` | `GET /networks/tron/transactions/{txHash}` | `rawTransaction` 为含全部签名的十六进制 protobuf 交易，用 txID 查询。 |
| Aptos | `POST /networks/aptos/transactions` | `GET /networks/aptos/transactions/{txHash}` | `rawTransaction` 为 Base64 编码的 BCS SignedTransaction，用用户交易哈希查询。 |
| Sui | `POST /networks/sui/transactions` | `GET /networks/sui/transactions/{txHash}` | 传 Base64 `rawTransaction`（TransactionData）和 Base64 `signatures` 数组，用 digest 查询。 |
| TON | `POST /networks/ton/transactions` | `GET /networks/ton/transactions/{txHash}` | `rawTransaction` 为 Base64 编码的外部入站消息 BOC，用服务返回的规范化消息哈希查询。 |

Solana 交易经核实过期且未入块后，发送失败回调，后续处理由应用决定。其他五条链各自的入块依据和 RPC 要求见下文。[Solana 代付与重签](#solana-代付交易) · [五条原生链的节点与入块依据](#原生链广播接口)

### Solana 代付交易

GM 的代付地址是 fee payer，用户授权转账。上游先确定集群、付款指令、代付地址和 recent blockhash，用户与 GM 的签名服务对**同一份 message** 签名，再提交完整序列化交易。Broadcaster 验证签名并广播。第一份签名属于 fee payer，也是 RPC 查询所用的交易 ID（下文记为 signature）。同一 GM 地址可同时代付多笔交易，各笔独立调度，提交顺序不保证链上执行顺序。本节适用于使用 recent blockhash 的交易；durable nonce 交易不适用这套过期规则。[Solana 交易结构](https://solana.com/docs/core/transactions/transaction-structure)

#### 提交与幂等

| 功能 | 方法与路径 | 输入 | 行为 |
|---|---|---|---|
| 提交已由用户和代付方签名的交易 | POST /solana/transactions | cluster、paymentId、signedTransaction（base64）、本次 getLatestBlockhash 返回的 context.slot 和 lastValidBlockHeight | 解析 message 中的 recentBlockhash，校验适用的交易格式、GM fee payer、用户及全部必需签名。缺签、签名无效或采用 durable nonce 时返回具体 4xx 原因；成功持久化返回 202、taskId 和 signature，直接进入待广播状态。 |
| 按签名查询支付进度与执行结果 | GET /solana/transactions/{signature}?cluster=… | 任一版本的第一份签名 | 返回同一支付任务、各版本签名和状态、blockhash、有效高度、逐供应商结果、入块 slot 和执行错误（如有）。 |
| 为过期支付提交重新签名的交易 | POST /solana/transactions/{signature}/resign | cluster、重新双签的完整原文和新 blockhash 的上下文及有效高度 | 仅当应用主动重试、所指版本仍是当前版本且已因 BLOCKHASH_EXPIRED 关闭时接收。核对同一付款、代付地址及指令与原版本一致，只更新 blockhash 和签名；在同一事务中登记新版本并调度。过时版本或旧交易结果待核实返回 409。 |

paymentId 由代付服务为同一笔业务付款稳定生成。按调用方、集群和 paymentId 唯一创建任务，按集群和 signature 去重。重交已登记的相同签名返回 200 及原任务；同一付款经普通入口提交另一份交易返回 409。重新签名接口重试已登记版本也幂等。恢复时保留所有版本，以旧 signature 查询仍能看到最终结果。

#### 广播与过期处理

| 当前状态 | 依据 | 下一步 |
|---|---|---|
| READY | 交易已全签并持久化，blockhash 仍有效 | 立即进入 BROADCASTING。对最多三家独立供应商并发调用 sendTransaction，发送相同的 base64 原文。两家或一家可用就立即发；零家可用时保持 READY 并继续监测有效期。 |
| BROADCASTING | 一家返回与本地一致的 signature | 进入 TRACKING，收集其他在途响应。RPC 接收只证明已提交；按 signature 查链上结果。 |
| BROADCASTING / TRACKING | 超时、断连、单节点报 BlockhashNotFound，或只观察到 processed | 继续按原 signature 核查。有效期内按需重播相同字节；拒绝原因按交易或端点归类。单个落后节点的响应不足以判定交易过期。 |
| 任意未结束状态 | 核实原 signature 在该集群达到 confirmed 或 finalized | 保存 signature 和 slot。err 为 null 表示成功，有错误值则报告失败及原生 err。缺少该字段时进入 CONFIRMING 并查执行元数据。暂停重播，按已见证的 slot 核查。 |
| READY / BROADCASTING / TRACKING | 同集群健康节点的 getBlockHeight 已**严格大于**该 blockhash 的 lastValidBlockHeight | 进入 CHECKING_EXPIRY，停止重播旧原文，核查旧签名是否已在有效期内入块。比较的是 block height；context.slot 只用于约束 RPC 数据新鲜度。 |
| CHECKING_EXPIRY | 历史记录查到旧签名已入块 / 结果有分歧或历史覆盖不足 | 查到入块但缺少执行结果时进入 CONFIRMING，取得结果后按 INCLUDED 关闭；证据不足保持核查，可见 processed 时继续追踪其归属。单次查无记录或墙钟超时均不足以要求重新付款。 |
| CHECKING_EXPIRY | 已核实旧 blockhash 失效，且有覆盖有效窗口的规范链历史证据表明旧签名未入块 | 以 `CLOSED / EXPIRED` 结束调度。向提交方发送 `transaction.closed`，原因 `BLOCKHASH_EXPIRED`，附签名和过期依据。交易未执行，省略 `executionStatus`。 |
| CLOSED / EXPIRED | 仅当应用自行决定重试，并通过 resign 接口提交新签名 | 在原 taskId、paymentId 下创建新版本，从 READY 开始调度。 |

签名方使用 getLatestBlockhash 一次取得 blockhash 及其 lastValidBlockHeight，建议使用 confirmed，广播预检采用一致的 commitment，并传 minContextSlot 避免落后节点误判。有效高度是签名原文之外的元数据；应核对消息中的 hash 与上游取得的配对记录。报告过期前，用健康节点核实同一旧 hash 在 finalized 视图中已失效、该视图已越过有效高度，再用 getSignatureStatuses（searchTransactionHistory=true）及具备历史覆盖的节点核查旧签名。无法确认旧交易结果时继续查询并对外报告原因。进程重启或通知重投都沿用已持久化的支付任务和签名版本。[获取 blockhash](https://solana.com/docs/rpc/http/getlatestblockhash) · [发送交易](https://solana.com/docs/rpc/http/sendtransaction) · [查询签名](https://solana.com/docs/rpc/http/getsignaturestatuses) · [交易确认与过期](https://solana.com/docs/advanced/confirmation)

#### 分叉与不完整的历史

服务在接单时，或首次观测到有效 blockhash 时，向 RPC 查询签名上下文所指的区块。只有该块的实际 Hash 与签名原文一致，才持久化其创建 slot。过期核查沿 finalized 区块的 parentSlot 和 previousBlockhash 回溯并检查每个区块的签名列表，进度可跨进程重启恢复。

| 观测 | 处理 |
|---|---|
| 核查期间 confirmed 视图再次证明原 hash 有效 | 恢复跟踪，按原签名重播；若其他节点已有过期的历史证据，则先处理分歧。 |
| 已见证的创建块后来成为孤块 | 最终链高度越过有效期、旧 hash 失效，且完整父链回溯覆盖创建 slot 后仍查无签名，可以报告过期失败。没有可靠的创建 slot 时，查到签名对应的原 blockhash 也能界定有效窗口。 |
| 节点仍返回 processed | 已验证的最终链历史覆盖该 slot 时可排除已丢弃分叉上的观测。processed 高于核查起点时，继续等待新 finalized 视图。 |
| 已见 confirmed 入块但缺少执行结果，此后签名从该分叉消失 | 保留已观察的 slot 和单独的历史扫描进度。只有 finalized 的完整排除证据覆盖该 slot 及原签名的整个有效窗口才报告过期失败。已核实 finalized 入块则继续等待执行结果。 |
| 历史缺块、父 Hash 不符或创建位置无法验证 | 保持 CHECKING_EXPIRY，保留可用进度，继续尝试其他节点。调用方提供的 slot、高度和一次空查询都不足以单独判定失败。 |
| 节点在 confirmed 层给出冲突的入块位置或执行结果 | 暂停关闭，等待一致视图或更强的 finalized 证据。 |
| 节点恢复后发现调用方在断网接单时给出的有效高度过大 | 用新鲜的 finalized blockhash 有效窗口识别矛盾，再以完整历史核查旧签名。 |

### 原生链广播接口

适用于 `bitcoin`、`tron`、`aptos`、`sui`、`ton`。请求须使用业务 Bearer Token。配置、准入和业务路径使用相同的网络键。

| 功能 | 方法与路径 | 行为 |
|---|---|---|
| 提交已签名交易并开始跟踪 | `POST /networks/{network}/transactions` | JSON 必填 `rawTransaction`。Sui 还须提供 `signatures` 字符串数组。首次创建返回 202，包含 `taskId`、`network`、`txHash` 和 `state`。同一调用方重交该标识返回 200 及原任务。 |
| 按交易标识查询进度与执行结果 | `GET /networks/{network}/transactions/{txHash}` | 按提交时返回的标识查询。响应包含任务状态、每次供应商广播的结果、入块证据及执行状态。其他调用方查询该任务返回 404。 |
| 验证并登记该链的供应商节点 | `POST /rpc-providers` | 管理员提供 `network`、`providerId`、`url` 和与上述 POST 请求体同格式的 `nativeProbe`。服务检查网络身份、读接口，并发送探针验证写权限。 |

| 网络 | 供应商 URL 与广播接口 | 查询标识与证据 |
|---|---|---|
| Bitcoin | Bitcoin Core JSON-RPC 2.0 网关，须启用并同步 txindex。发送用 `sendrawtransaction`。如节点使用 Basic Auth，可由私有网关注入凭证。 | 本地计算 txid，隔离见证交易同样按 txid 查询。核对主网创世哈希，使用 `getrawtransaction` 和 `getblockheader` 查入块，再用 `getblockhash(height)` 核对该高度当前主链上的区块。 |
| TRON | 配置可访问 Wallet 和 WalletSolidity 的根 URL。交易按 protobuf 编码为 hex，通过 `/wallet/broadcasthex` 发送。 | txID 为原始交易数据的 SHA-256。读取固化视图的交易回执，并核对指定块中确有同一 txID。 |
| Aptos | 配置完整的 fullnode REST API 根路径，通常以 `/v1` 结尾。将 Base64 解码后的原始 BCS 字节发送至 `/transactions`。 | 对原始签名交易按 Aptos 用户交易的域前缀计算哈希。节点校验签名和链 ID。按哈希查询，pending 继续跟踪，已提交的 user_transaction 返回账本版本及 success。 |
| Sui | 配置 gRPC v2 的 HTTP(S) 主机 URL。使用 `ExecuteTransaction` 传递 TransactionData 和全部签名，多个签名按原样发送。 | 根据 TransactionData 原始字节计算 digest。`GetTransaction` 同时返回相同 digest、checkpoint 及 effects.status 后关闭。 |
| TON | 配置同时提供 TON Center v2 写接口和 v3 索引的网关根 URL。调用 `/api/v2/sendBoc` 提交单根外部入站消息。 | 返回小写十六进制的规范化消息哈希。使用该哈希查询 v3 索引，核对返回的入站消息规范化哈希及块位置，返回接收方账户交易哈希和逻辑时间。 |

例如，Sui 请求体形如 `{"rawTransaction":"<base64 TransactionData>","signatures":["<base64 UserSignature>"]}`。另外四条链只传 `rawTransaction`。签名服务负责 Bitcoin 的输入选择、TRON 的交易有效期、Aptos 的账户序号、Sui 的对象引用以及 TON 钱包的重放保护。

TRON 返回 `TRON_SIGNATURE_WEIGHT_INSUFFICIENT` 时，可在原交易中保留已提交的全部签名、补齐授权签名，再向同一提交接口发送。接受补签后返回 202，沿用原任务和 txID。重试旧签名子集返回原任务，已补签名继续保留。签名原文或签名集合不兼容时返回 409 `SIGNED_ENVELOPE_CONFLICT`。

原生链任务从 `QUEUED` 开始调度。提交给节点后进入 `TRACKING`，复用三供应商策略、SQLite 重启恢复和 Webhook。读取不完整或节点对入块位置、执行结果存在分歧时继续核查。已核实入块但仍缺执行元数据时进入 `CONFIRMING / PENDING`。入块证据及执行结果一致时进入 `CLOSED / INCLUDED`，单独记录执行成功或失败。超过 `trackingHours` 仍未核实入块则以 `CLOSED / EXPIRED` 结束本地跟踪；需要重付时先核对原交易的链上结果。

## EVM 交易生命周期

核实交易在规范区块中后，Broadcaster 停止重播并保存区块证据。若同轮已取得执行结果，直接关闭并回调提交方。只有入块证据时，任务进入 `CONFIRMING`，`executionStatus=PENDING`，暂不设置 `taskOutcome`。后台继续查回执，重启后自动恢复。此时禁止强制广播、替换和过期重签。已关闭任务保留当时的链上快照。

### 如何判断失败原因

| 阶段 | 字段或事件 | 例子 |
|---|---|---|
| 提交 | HTTP 错误体的 `error` | `NONCE_CONFLICT` 表示同一账户的本地活动任务已占用该 Nonce。已核实由其他交易消耗时返回 `NONCE_CONSUMED`。 |
| 广播 | 每条 `attempts[].broadcasts[].reason`、`detail` | EVM 可以返回 `NONCE_TOO_LOW`、`INSUFFICIENT_FUNDS`、`INTRINSIC_GAS_TOO_LOW` 或 `FEE_TOO_LOW`。TRON 可区分 `TRON_SIGNATURE_WEIGHT_INSUFFICIENT`、`TRON_PERMISSION_DENIED` 和 `TRON_SIGNATURE_INVALID`。一路接收即继续跟踪。 |
| 确认外部覆盖 | `transaction.nonce_conflict` 和 `transaction.closed` 的 `data.reason` | `NONCE_CONSUMED_EXTERNALLY` 附消耗该 Nonce 的交易和区块。 |
| EVM 执行 | `executionStatus=FAILED`，`reason` | 通过失败交易的顶层 trace 区分 `EVM_OUT_OF_GAS`、`EVM_REVERTED` 和 `EVM_VM_ERROR`。有回退说明时见 `evidence.revertReason`。只有失败回执时返回 `EXECUTION_CAUSE_UNAVAILABLE`。 |
| 其他链执行 | `executionStatus=FAILED`，`reason` 和 `evidence` | TRON 可报 `TRON_OUT_OF_ENERGY`、`TRON_REVERTED`；Solana 保留原生 `err`（如指令下标和程序错误码）；Aptos 保留 VM 状态文本；Sui 返回节点提供的错误类型；TON 可附计算或动作阶段的退出码。 |

此前没有接收记录且本轮全部明确拒收时，查询可查看每个供应商的诊断。回调的 `data.evidence.providerReasons` 列出本轮各家供应商的原因。首次出现该拒收原因或原因改变时，提交方收到 `transaction.broadcast_rejected`；需要加价时收到 `transaction.replacement_required`。入块后的 `transaction.closed` 用 `data.reason` 和 `data.evidence` 报告执行失败。历史版本已发送的关闭事件保留原语义，旧的 `CLOSED / INCLUDED / UNKNOWN` 仍可按哈希补查。[EVM 回执](https://eips.ethereum.org/EIPS/eip-658) · [Geth 调用跟踪](https://geth.ethereum.org/docs/developers/evm-tracing/built-in-tracers) · [TRON 签名权限](https://developers.tron.network/docs/multi-signature)

~~~mermaid
flowchart TD
    Submit["提交签名原文，按任务唯一键判重"] --> Q["QUEUED"]
    Q -->|具备调度条件| T["TRACKING：最多三家广播，监听所有候选"]
    T -->|显式 replacement| T
    Q -->|发现 Nonce 已越过| R["RESOLVING_NONCE：暂停该槽重播"]
    T -->|发现 Nonce 已越过| R
    T -->|规范区块视图分歧| R
    R -->|链回退且仍缺前序 Nonce| Q
    R -->|重新核实该值可调度| T
    Q -->|候选已在块内| B{"执行结果已取得？"}
    B -->|是| I["CLOSED / INCLUDED"]
    B -->|否| C["CONFIRMING / PENDING"]
    C -->|后台核实成功或失败| I
    C -->|复核证实原块已重组且可重新调度| T
    T -->|候选已在块内| B
    R -->|本任务任一候选在块内| B
    R -->|证实外部消耗| X["CLOSED / REPLACED_EXTERNALLY"]
    X --> N["通知提交方，重排该账户的剩余任务"]
    I --> A["通知提交方，唤醒该账户后续任务"]
    Q --> E["永久拒绝或期限到达时关闭"]
    T --> E
    R -->|期限到达| E
~~~

### 任务状态转换

| 当前状态 | 转换条件 | 下一状态与动作 |
|---|---|---|
| 尚无任务 | 首次提交通过校验，已持久化原文、任务键和调度信息 | QUEUED。重复 Hash 返回原任务；本地任务键冲突返回 409 NONCE_CONFLICT；已核实链上越过提交值，且非本任务候选入块时返回 409 NONCE_CONSUMED。 |
| QUEUED | 账户排序允许（或有效 force），且至少一家可发送 | TRACKING，发送首轮。缺前序 Nonce 或没有可用节点时保持 QUEUED，记录原因。 |
| TRACKING | RPC 返回接收、拒绝、超时，或签名方提供替换交易 | 保持 TRACKING，更新对应 Hash 和节点记录。替换版本开始调度，旧版本停止主动重播，仍参与入块核查。 |
| QUEUED / TRACKING | 规范链账户 Nonce 已大于本地交易 Nonce，尚未核实消耗者 | RESOLVING_NONCE。暂停该任务调度；查所有候选及区块。 |
| RESOLVING_NONCE | 证据不足，或复核发现此前读数已失效 | 有分歧时保持核查。链上 Nonce 回退后，若缺前序值则回 QUEUED / NONCE_GAP，具备发送条件则回 TRACKING。 |
| 任意未关闭状态 | 只核实任务内某个 Hash 入块，缺少执行回执 | CONFIRMING / PENDING。自动复查，不发结束回调。 |
| 任意未关闭状态 | 在当前规范区块核实任务内某个 Hash 和执行结果 | CLOSED / INCLUDED，记录 winningHash 及独立执行结果，其他候选记 REPLACED。通知上游并更新账户队列。 |
| RESOLVING_NONCE | 定位到任务外交易消耗该 Nonce（含已生效的 7702 授权） | CLOSED / REPLACED_EXTERNALLY。返回错误 NONCE_CONSUMED_EXTERNALLY、受影响的本地 Nonce 和 Hash、消耗者及区块，停止全部候选。通知原提交方并重排队列。 |
| 尚未核实入块 | 所有候选永久无效且证据充分 / 配置的追踪期限到达 | 分别以 CLOSED / REJECTED 或 CLOSED / EXPIRED 结束。后者保留最后观测，已发原文仍可能上链。 |

已接收任务的外部覆盖通过 GET 任务结果和异步事件报告。核查期间查询可见 RESOLVING_NONCE；单个 RPC 拒绝、空回执、链上 Nonce 增长都不能单独证明“外部覆盖”。已绑定 Hash 的本地任务历史可追溯；新的签名交易不得复用被链上消耗的槽位。

**重组与供应商分歧。** 每轮并行核查可读端点。发现候选入块或外部消耗时，在回执查询、7702 诊断和历史查询完成后，重新按高度读取见证节点的规范块，并比对其他可读节点在该高度的区块 Hash。任一节点给出不同的块、候选赢家或明确相反的执行结果，任务进入 `RESOLVING_NONCE / CHAIN_VIEW_CONFLICT`，暂停重播，下一轮复核。落后节点尚无该块或暂时读不到时，可依据一家完成核查的供应商关闭。一个旧候选的孤块回执不会挡住新候选的核查。任务保存获胜 Hash、区块及观测时间，只向上游报告一次逻辑关闭事件。关闭之后发生重组，查询仍返回已保存的入块快照；依赖最终性的结算方需自行等待目标链的最终性条件。[EIP-1898 对跨 RPC 读取一致性的说明](https://eips.ethereum.org/EIPS/eip-1898)

确认执行期间若原区块被替换，节点对同一高度的新规范块达成一致，且账户状态允许重新调度，任务回到 `TRACKING / INCLUSION_REORGED`。临时读不到回执时继续保持确认中。

### 签名交易与节点请求

| 当前 Hash 状态 | 条件 | 下一状态与动作 |
|---|---|---|
| QUEUED | 被选为当前版本且可发送 | BROADCASTING。持久化轮次再并发发送；未满足条件继续排队。 |
| BROADCASTING | 一家接收或明确已知该 Hash | PENDING，继续收集同轮其他结果。 |
| BROADCASTING | 至少一路结果不明且无明确接收 | SUBMISSION_UNKNOWN。按本地 Hash 核查，避免在超时后误报失败。 |
| BROADCASTING | 全部明确拒绝 | 可恢复错误回 QUEUED，记录退避或阻塞原因；提示 Nonce 过低则触发账户对账；永久原文错误记 REJECTED。 |
| SUBMISSION_UNKNOWN | 查到 pending / 暂不可见且 Nonce 可用 | 前者转 PENDING；后者当前版本按策略回 BROADCASTING，旧版本保持观察。 |
| PENDING | 仍未入块 | 保持 PENDING。可重播同一原文；费用不足时通知签名方提供 replacement。新一轮超时保留先前接收证据。 |
| 任意仍可能入块的候选 | 本 Hash 入块 / 同任务其他 Hash 入块 / 外部消耗得到核实 | 仅见入块时记 CONFIRMING。执行结果明确后分别记 INCLUDED 或 REPLACED，外部消耗核实后记 REPLACED_EXTERNALLY。 |
| 任意仍在追踪的候选 | 任务到期 | 停止主动调度和监控，保留最后观测。 |

每条节点请求从 IN_FLIGHT 转为 ACCEPTED、ALREADY_KNOWN、REJECTED 或 UNKNOWN。记录轮次、端点、供应商、时间、延迟、错误分类及脱敏原始错误。迟到响应只补充节点记录，已关闭任务保持原结果。

按任一候选 Hash 查询，返回 taskId、taskState、taskOutcome、chainId、sender、nonce、queriedHash、activeHash、winningHash、consumingHash、attempts 及阻塞原因。入块附 completionBasis=BLOCK_INCLUSION、区块证据、executionStatus、receipt 和逐条 authorizations。外部覆盖时返回错误及链上证据。实时账户 Nonce 可另行查询。

## 故障恢复与上游事件

接收事务写入任务、签名原文和调度信息后再响应。工作实例以租约领取到期任务；重启后接手失效租约，先核查全部候选 Hash，再决定按入块关闭、等待或重播。实际 RPC 调用允许重复，数据库对任务、候选和事件去重。SQLite 保留恢复所需的原文、状态与证据。

任务状态和待发事件通过同一事务写入 SQLite。每个提交服务独立接收已签名的 HTTPS Webhook。接收端以共享密钥对**原始请求体字节**计算 HMAC-SHA256，用恒定时间比较其十六进制摘要与 `X-Broadcaster-Signature`。先保存事件，再返回 2xx。

| 事件 | 接收方如何处理 |
|---|---|
| `transaction.submitted` | 记录已保存的任务或新签名版本。 |
| `transaction.broadcast_rejected` | 查看节点的拒收原因。 |
| `transaction.replacement_required` | 核对费用并决定是否签署同 Nonce 替换交易。 |
| `transaction.included` | 显示已见入块，同时检查执行状态。 |
| `transaction.nonce_conflict` | 核查占用该 Nonce 的外部交易。 |
| `transaction.closed` | 根据 `data.taskOutcome`、`data.executionStatus`、`data.reason` 更新订单。 |

通知包含 `eventId`、`event`、任务版本 `version` 和快照 `data`。`Idempotency-Key` 与 `eventId` 相同。按事件 ID 去重，按任务版本防止旧快照覆盖新状态。同一次状态变更可能发出相同版本号的 included 和 closed，两种事件各自处理。回调失败只重试该事件。429 或 503 响应可通过 `Retry-After`（秒数或 HTTP 日期，上限 24 小时）延后重试。一家接收服务响应慢时，其他提交方继续接收自己的通知。恢复后可按原哈希补查。

## RPC 节点管理

每个端点归属于一个稳定的 providerId 和一条链。同一供应商的多个域名、密钥或端点只占一路。每个 EVM 网络使用独立的节点池，可组合 PublicNode 和自购端点，所有端点须能广播普通 EVM 交易及该链配置启用的交易类型。除采用 Cancun 规则的 HyperEVM（999）外，准入另需通过 EIP-7702 Type 4 广播探针。配置了 EntryPoint 的链还须通过相应的 ERC-4337 外层交易探针。4337 外层交易调用目标链的 EntryPoint 合约，经 eth_sendRawTransaction 提交。面向钱包的 eth_sendUserOperation 由独立 Bundler 提供。

| 功能 | 方法与路径 | 行为 |
|---|---|---|
| 验证并登记供应商节点 | POST /rpc-providers | 管理员提交 providerId、chainId、端点和凭证配置。验证实际 chainId、鉴权、查询与广播权限、普通交易和该链启用的 Type 4 广播能力，以及对已配置 EntryPoint 的 4337 外层交易广播权限。全部通过才登记启用并返回 201。失败或能力无法证实时返回具体检查项与原因。 |
| 查看节点健康与熔断状态 | GET /rpc-providers?chainId=… | 返回供应商和端点标识、启停状态、最近高度及观测时间、读和广播健康状况、失败计数、冷却截止时间与最近错误。凭证与 URL 中的密钥须脱敏。 |
| 修改、启用或停用节点 | PATCH /rpc-providers/{id} | 以端点记录 ID 修改配置或启停。端点、凭证或链变更时先重新准入，检查通过后原子切换；失败时原配置继续生效。停用后停止分配新请求。 |
| 查看各链内置节点 | GET /rpc-providers/defaults?network=… | 返回配置网络的默认地址、providerId 和 MEV 保护标记。省略 network 时列出全部预设。 |

准入须有目标端点和对应凭证的能力证据。例如可使用运维提供的受控已签名测试交易，验证对应写接口接受该类型；费用及链上副作用须在准入测试安排中明确。对 4337，按目标链登记 Bundler 使用的 EntryPoint 地址和版本，核对合约部署，并验证执行端点允许发送有效的、调用该 EntryPoint 的已签名 bundle。查高度成功、能读取历史 Type 4 交易或收到通用参数错误，均不足以证明该端点有 Type 4 广播权限。

Solana 按集群单独建池。准入时用 getGenesisHash 核对集群，验证凭证、sendTransaction 写权限以及交易状态、历史和区块高度查询能力。以 getBlockHeight 和节点进度做读侧健康检查。18 条链共用下述供应商去重、广播熔断、半开试发及恢复规则。

<a id="publicnode-默认节点"></a>

### 内置主网节点

内置以下主网端点。管理员可通过 `GET /rpc-providers/defaults` 查看当前配置网络的默认 URL 和 `mevProtected` 标记；可选 `?network=1` 过滤。EVM 按 chain ID 匹配，Solana 按 mainnet-beta 的 genesis hash 匹配，配置中的集群名称可自定义。保护标记来自供应商对相应 HTTPS RPC 地址的公开说明。[PublicNode 官方端点目录](https://www.publicnode.com/)

| 网络 | HTTPS RPC | 供应商标记 |
|---|---|---|
| Ethereum · 1 | `https://ethereum-rpc.publicnode.com` | MEV-Protected |
| BNB Smart Chain · 56 | `https://bsc-rpc.publicnode.com` | MEV-Protected |
| Base · 8453 | `https://base-rpc.publicnode.com` | MEV-Protected |
| Arbitrum One · 42161 | `https://arbitrum-one-rpc.publicnode.com` | MEV-Protected |
| Solana · mainnet-beta | `https://solana-rpc.publicnode.com` | MEV-Protected |
| Arc · 5042 | `https://arc-rpc.publicnode.com` | MEV-Protected |
| Robinhood Chain · 4663 | `https://robinhood-rpc.publicnode.com` | MEV-Protected |
| HyperEVM · 999 | `https://hyperliquid-rpc.publicnode.com` | 普通 RPC |
| Bitcoin | `https://bitcoin-rpc.publicnode.com` | Bitcoin Core RPC |
| TRON | `https://tron-rpc.publicnode.com` | Wallet 和 WalletSolidity HTTP |
| Aptos | `https://aptos-rest.publicnode.com/v1` | Fullnode REST |
| Sui | `https://sui-grpc.publicnode.com:443` | 原生 gRPC v2（TLS） |
| OP Mainnet · 10 | `https://optimism-rpc.publicnode.com` | 普通 RPC |
| Polygon PoS · 137 | `https://polygon-bor-rpc.publicnode.com` | 普通 RPC |
| Avalanche C-Chain · 43114 | `https://avalanche-c-chain-rpc.publicnode.com` | 普通 RPC |

另外三条主网使用项目或官方 API 提供方公布的地址，均按普通广播端点调度。

| 网络 | providerId | HTTPS 地址 | 官方资料 |
|---|---|---|---|
| Monad · 143 | `monad-foundation` | `https://rpc-mainnet.monadinfra.com` | [Monad Foundation 公共 RPC](https://docs.monad.xyz/developer-essentials/network-information) |
| Plasma · 9745 | `plasma` | `https://rpc.plasma.to` | [Plasma 主网参数](https://www.plasma.org/docs/guides/network-configuration/mainnet-details) |
| TON | `toncenter` | `https://toncenter.com` | [TON 官方 API 提供方](https://docs.ton.org/nodes/overview) · [TON Center v3](https://docs.ton.org/ecosystem/api/toncenter/v3/overview) |

TON 使用同一主机的 v2 发送消息、v3 按消息哈希查询，只占一家供应商。三条公开端点均有限流。Monad Foundation 将该端点列为开发用途，生产部署应使用适合生产流量的供应商。[Monad 使用说明](https://docs.monad.xyz/developer-essentials/network-information)

**启用。** 向 `POST /rpc-providers` 提交默认目录中对应的 `providerId`、目标 `network` 和该链的签名准入探针，服务自动选择 URL。例如以太坊请求体形如 `{"providerId":"publicnode","network":"1","evmProbes":["0x…","0x…","0x…"]}`。首次通过全部检查后进入调度池；重复提交返回原记录。运行中的节点和启停状态可通过 `GET /rpc-providers` 查询。

**启动时自动准入。** 在相应网络配置中将 `defaultProbeFile` 指向 JSON 文件（兼容旧配置键 `publicNodeProbeFile`），路径相对于配置文件。示例配置的默认路径为 `probes/<网络键>.json`。EVM 文件包含 `evmProbes`，Solana 文件包含 `solanaProbe`，Bitcoin、TRON、Aptos、Sui 和 TON 文件包含 `nativeProbe`，格式与上述准入 API 相同。首次启动使用文件中已获授权的签名原文做真实准入；失败原因写入日志，可更新探针后通过 API 再次准入。重启复用 SQLite 中的记录及人工停用状态。

生产管理员核对付费供应商对具体广播地址的说明后，可在 POST 或 PATCH 中设置 `mevProtected: true`。更换供应商、网络、URL 或凭证时需重新声明该标记。内置端点的标记由目录决定。

**选点。** 在启用、健康且允许试发的端点中，优先选择带保护标记的地址，再补齐最多三家独立供应商。每轮 PublicNode 只占一路，冷却时可由其他可用供应商补位。MEV 保护作用于经过该端点的一路提交；同一交易并发发送到普通 RPC 时，仍可能进入公开交易池。

### 每轮选点

按链筛选已启用、高度检查可用且广播未冷却的供应商。每家选一个端点，同轮立即发送同一份签名原文。三家或以上发三家，两家发两家，一家即发一家。零家可用时保留调度请求，记录 NO_AVAILABLE_PROVIDER，最早可试发的端点恢复时唤醒。已广播的候选继续监控。第一家接收后，其他已发出的请求照常收集结果。

### 高度检查、广播熔断与恢复

EVM 使用 eth_blockNumber 记录端点延迟、最后高度和最近推进时间，并对比同链独立供应商。单次高度持平是正常现象；持续落后或停止推进按链的出块节奏判定。只有一家时参考该端点自身近期推进情况。高度检查和广播各自记录健康状态，高度探测成功只能恢复读侧健康。[eth_blockNumber](https://eips.ethereum.org/EIPS/eip-1474#eth_blocknumber)

下表是**每条链、每个端点的广播熔断器**。冷却阶梯为 3、6、10、15、21、28、36、45、55、60 分钟，之后保持 60 分钟。每档都需要三次端点故障；首次 3 分钟是 Broadcaster 的约定。阶梯后段及上限依据 [ChainScanner 的计数实现](https://github.com/GMWalletApp/ChainScanner/blob/87d802f3daa946e265ed8e396378fb13884a5eee/internal/nodes/source_pool.go#L57-L72)。

| 当前状态 | 条件 | 下一状态和动作 |
|---|---|---|
| HEALTHY | 收到可归因于该端点的超时、断连、限流、凭证失效或服务端故障 | 前两次保持 HEALTHY 并累计；第 3 次进入 COOLDOWN，记录截止时间，当前档计数归零。 |
| COOLDOWN | 未到期 | 跳过新广播；保留每条在途请求的真实结果。过期轮次的迟到响应不能提前解除冷却。 |
| COOLDOWN | 已到期且有待发送交易 | 进入 HALF_OPEN，跨实例只放行一个实际广播试发。无交易时等待下次调度。 |
| HALF_OPEN | 单次试发发生端点故障 | 前两次保持 HALF_OPEN，每次只放行一个试发；第 3 次进入下一档 COOLDOWN。 |
| HEALTHY / HALF_OPEN | 接收并返回预期 Hash，或明确返回 already known | 进入 HEALTHY，清零失败次数和冷却档位。 |
| HEALTHY / HALF_OPEN | 余额不足、替换加价不足、nonce too low 等交易级拒绝 | 记录到该 Hash，不增减端点故障计数；半开时释放试发名额。nonce too low 触发链上 Nonce 核查。 |

计数、档位、截止时间及半开试发租约需跨实例共享，按端点原子更新。三家并发各计各的。一家返回预期 Hash，另一家返回 already known，第三家超时，本轮交易即可进入 PENDING，只有第三家增加一次故障。already known 表示该节点已知这笔交易。[Geth 交易池错误定义](https://github.com/ethereum/go-ethereum/blob/master/core/txpool/errors.go)

<a id="首版验收示例"></a>

<a id="balancekeeper"></a>

## 向 BalanceKeeper 推送全局广播通知

现有 `clients[].webhook` 按交易提交方投递任务事件。服务支持一个可选的全局 HTTPS 接收端，覆盖所有提交方。运维配置一次接收 URL 和独立密钥即可。钱包的地址清单由 BalanceKeeper 自己维护。[BalanceKeeper 接收契约](https://github.com/GMWalletApp/BalanceKeeper/blob/main/docs/api.zh-CN.md#broadcaster)

| 事件 | 何时产生 | 哈希 |
| --- | --- | --- |
| `broadcast.accepted` | 同一签名版本第一次持久化节点的 `ACCEPTED` 或 `ALREADY_KNOWN` 回执。仅 API 入队（HTTP 202）、超时或拒绝均不触发。 | `hash` 为该版本的链原生广播标识。 |
| `broadcast.included` | 核实实际胜出版本已入块，包括各次发送均超时、恢复后才查到入块，以及入块后执行失败的交易。 | `hash` 为胜出版本标识，另附实际链上交易 `txHash`。TON 用它关联外部消息与交易。 |

**请求。** 向 BalanceKeeper 的 `POST /v1/notifications/broadcasts` 发送非空 JSON 数组。每项必填下例字段。`schemaVersion` 为整数 1，`eventId` 是重投时保持稳定的不透明标识；`family`、`network` 使用 Broadcaster 的链家族及配置键，EVM 的 network 为十进制 chain ID 字符串。`hash` 保留原生编码和大小写：Solana 用签名，Sui 用 digest，Bitcoin 用 txid，TON 用规范化外部消息哈希的十六进制。入块事件必填 `txHash`，TON 使用链上 32 字节交易哈希的标准 Base64。全局载荷只包含这些路由字段和标识。

```json
[
  {
    "schemaVersion": 1,
    "eventId": "8af9bce60622455c8710516cde6d9981",
    "event": "broadcast.accepted",
    "family": "evm",
    "network": "8453",
    "hash": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  },
  {
    "schemaVersion": 1,
    "eventId": "51a0053ad0c34a84834dbee02ab77d69",
    "event": "broadcast.included",
    "family": "evm",
    "network": "8453",
    "hash": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "txHash": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  }
]
```

请求头为 `Content-Type: application/json` 和 `X-Broadcaster-Signature`。签名值为对原始正文使用独立密钥计算的 HMAC-SHA256 小写十六进制。在现有配置文件的顶层加入以下字段，并将同一个环境变量配置给 BalanceKeeper 的 `broadcastSecret`。启用后，新的节点接收和入块记录会生成通知。省略此配置即可关闭投递。

```json
{
  "broadcastNotifications": {
    "url": "https://balancekeeper.example.com/v1/notifications/broadcasts",
    "secret": "${BROADCASTER_NOTIFICATION_SECRET}"
  }
}
```

**投递。** 首笔合格的节点回执或入块证据与全局待发事件在同一 SQLite 事务内提交。同一哈希被多个节点接收或重复广播只触发一次接收事件；签名替换产生新哈希。重组后同一哈希在不同规范块重新入块，使用新的入块事件 ID。全局接收端有独立的投递进度，慢接收端不会阻塞广播或原提交方回调。允许至少一次、乱序及重新分批投递，每项保持原事件 ID。

BalanceKeeper 整批持久化后返回 204，已收到的事件也可确认。超时、429、5xx 或响应丢失时保留待投递状态并退避重试，429/503 遵循 `Retry-After`。401 或 400 时保留未确认批次并报告配置或协议错误。

## 验证与维护

在仓库根目录运行以下命令。测试使用临时数据库和本地模拟 RPC。

```sh
go test -race ./...
go vet ./...
```


| 输入或故障 | 预期可观察结果 |
|---|---|
| 同一 Nonce 并发提交两份不同原文，随后重试胜出的原文 | 一个任务、一个 409；重试返回原任务。只有 replacement 能登记第二个 Hash。 |
| 三家分别返回预期 Hash、already known、超时 | 一笔 PENDING 交易、三条广播记录，只有超时端点增加失败计数。 |
| 某端点连续三次超时；3 分钟内读高度正常；到期后三次试发再超时 | 广播先冷却 3 分钟，再冷却 6 分钟。读请求不重置广播计数。随后一次试发明确接收即可复位。 |
| 可用供应商从三家降到两家、一家、零家 | 实际发出 3、2、1、0 路。零家时任务可恢复，端点可试发后自动唤醒。 |
| 全部请求超时且进程重启，随后查到已上链回执 | 复用原任务，由 SUBMISSION_UNKNOWN 进入 INCLUDED，以观测到的入块证据关闭。 |
| A 广播后显式替换为 B，先在规范区块找到 A，外层回执为 0 | 两个 Hash 均返回 CLOSED / INCLUDED、winningHash=A、executionStatus=FAILED，B 为 REPLACED。关闭后无重组恢复任务。 |
| Type 4 外层成功但一条授权被跳过 | 外层 SUCCESS；能取得交易级证据时列出被跳过的条目和原因，证据不足时报告 UNKNOWN。 |
| 用户自行消耗队列中某 Nonce，节点最初只报 nonce too low | 先核查全部候选。找到外部交易后原任务和通知均带冲突原因，重排剩余任务，较高的已签名交易保持原 Nonce。 |
| 4337 bundle 包含两笔操作，外层入块且执行成功，其中一笔操作执行回退 | Broadcaster 返回外层 INCLUDED 和 winningHash；Bundler 按两个 userOpHash 分别返回 success=true 和 success=false。 |
| 首次代付前，A 在外部钱包先花掉授权 Nonce | 标记 A 的授权需核查或重签；已经发出的 R 交易仍按外层 Hash 独立跟踪。 |
| 同一 R 并发代付两笔，第一笔提交响应超时 | 两笔使用不同的 R 账户 Nonce；重试第一笔复用其原文和 Hash。若同属 A 且 D 采用顺序操作 Nonce，还需验证两份指令各有独立、顺序可执行的编号。 |
| 代付地址 R 的 RPC latest 返回 20，本地队列已有尚未广播的 20 | 查询返回 chainNonce=20、nonce=21（假设 21 可用）。另一个 Hash 普通提交同一个 20 时返回 409；查询和冲突均不改变任何账户计数器。 |
| Solana 同一 GM fee payer 同时代付两笔已全签交易，其中一笔三家发送均超时 | 两笔独立调度。超时的交易按本地第一份签名查链；有效期内重播相同字节。 |
| Solana 旧 blockhash 已过期，历史查询随后查到原签名已入块 | 关闭原任务并返回执行结果，重试原签名返回同一结果。 |
| Solana 旧签名确认未入块且 blockhash 已失效，上游只提供新的 GM 签名 | 缺用户的新签名时拒绝；双方对新消息签齐后才登记新版本。重复提交同一新签名只产生一个版本。 |
| 回调返回 429 或某提交服务一直响应缓慢 | 其他提交方继续收到通知。出错的事件按原 eventId 重试。 |

### 代码位置

| 修改目标 | `internal/broadcast/` 下的文件 |
|---|---|
| 接口、验签、账户队列 | `http.go`、`signed.go`、`nonce.go`、`store.go` |
| 任务调度与状态提交 | `worker.go` 领取任务，`dispatch.go` 记录各路广播，`transition.go` 提交状态和事件。 |
| 链上证据 | EVM 见 `observe.go`、`receipt.go`、`reconcile.go`。Solana 见 `solana.go` 和 `solana_expiry.go`。其他五条链见各自的 `native_<chain>.go`。 |
| 回调投递 | `events.go` 负责投递，`schema.sql` 定义待发事件表及索引。 |
