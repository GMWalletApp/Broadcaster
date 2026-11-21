# 接口与交易生命周期

[返回中文首页](../README.zh-CN.md) · [18 条主网配置](../config.mainnets.json)

在 EVM 上，签名方自行查询和选取 Nonce、构造交易并签名，也负责签署加价替换版本。Broadcaster 从已签名原文恢复外层发送地址和 Nonce，按账户调度已接收任务。用户仍可持私钥在其他钱包发交易。Broadcaster 处理普通 EVM 交易和 EIP-7702 Type 4 交易，包括逐条授权诊断。独立的 ERC-4337 Bundler 接收并打包 UserOperation；Broadcaster 接收 Bundler 签好的外层交易，负责多供应商广播和入块通知。

## 运行

需要 Go 1.26.2 或更高版本、本地可写目录和通过准入的 RPC 节点。以下命令构建服务并在本机启动。配置示例启用以太坊；接入 Solana 时，在 networks 增加集群名、family（solana）、genesisHash 及允许代付的 feePayers 公钥数组。

~~~sh
go build -o broadcaster ./cmd/broadcaster
cp config.example.json config.json
export BROADCASTER_API_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_ADMIN_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_STORAGE_KEY="$(openssl rand -base64 32)"
./broadcaster -config config.json -db broadcaster.db
~~~

首次生成的凭证和存储密钥应交给部署环境保存，重启时继续使用。API 使用 Bearer Token；admin 角色管理供应商，业务令牌仅能查询和修改自己的任务。网络和代付公钥配置从文件加载。监听地址默认 127.0.0.1:8080，对外服务可在反向代理终止 TLS。验证命令为 `go test -race ./...` 和 `go vet ./...`，集成测试使用临时 SQLite 文件和本机模拟 RPC。

**数据库。** 程序在启动时创建表，启用 WAL、外键、5 秒 busy timeout 和 FULL 同步。签名原文、任务版本、节点冷却、广播历史及待发通知写入同一文件。变更状态使用短写事务，所有 RPC 及 Webhook 请求在事务外执行。多个 worker 通过带期限的租约领取任务，进程意外退出后其他 worker 可接手。同机进程须访问同一本地数据库并使用相同密钥。备份运行中的库请使用 SQLite 在线备份；停机复制前须完成 WAL 检查点。RPC 地址及 Bearer 凭证采用 AES-256-GCM 加密保存。

**节点准入。** 管理员向 POST /rpc-providers 提交 network（或 chainId）、providerId、url、可选 bearer。在 EVM 网络附 evmProbes（受控的已签名交易数组），覆盖普通交易、该链启用的 Type 4，以及配置的每个 EntryPoint 的 bundle。Solana 网络附 solanaProbe，结构同 Solana 提交请求。Bitcoin、TRON、Aptos、Sui 和 TON 使用 nativeProbe，结构同该链的提交请求。准入会真实发送这些原文，使用专用探测账户和已批准的测试操作。添加成功后响应中的 id 用于 PATCH；仅修改 enabled 无须重新发送探测交易。

**事件通知。** 在 clients 的相应条目配置 webhook（HTTPS）和 webhookSecret 可启用异步通知，密钥支持环境变量。请求体包含 eventId、event、version、data。接收方使用原始请求体和共享密钥计算 HMAC-SHA256，与 X-Broadcaster-Signature 的十六进制值核对，以 eventId 去重。

**实现范围。** EVM 解析受链 ID 保护的 legacy、Type 1、2、4。ERC-4337 支持 Bundler 签好的外层交易。Solana 接收 legacy 和 v0 完整签名交易。7702 逐项确证需要节点的交易前状态跟踪能力；提供方无此能力时保留 UNKNOWN。Solana 历史不完整时保持 CHECKING_EXPIRY，服务不会自行签名。

## EVM 调用方接口

交易提交和管理操作需要鉴权，查询和变更限定在调用方有权访问的任务或账户内。POST 和 PATCH 的请求体使用 JSON。下表中的 rawTransaction 是完整的十六进制已签名原文。Hash 路径支持用 chainId 查询参数限定网络；出现跨链歧义时必须指定链。

| 方法与路径 | 输入 | 响应与副作用 |
|---|---|---|
| POST /transactions | rawTransaction | 解析签名、链、发送方、外层 Nonce 和本地 Hash。首次持久化返回 202 及 taskId、txHash、state。重复提交相同 Hash 返回 200 及已有任务。同链、同发送方、同 Nonce 已由其他活动任务占用时返回 409 NONCE_CONFLICT。 |
| GET /transactions/{hash} | 原始或替换 Hash | 返回 200，包含任务阶段和结果、所查 Hash 的状态、当前调度版本、全部候选、各轮各节点结果、链上证据及 7702 诊断。未知或不可见的 Hash 返回 404。 |
| POST /transactions/{hash}/force | reason | 对当前可调度版本立即安排一轮广播，记录操作者、时间和原因，返回 202。只跳过本地 Nonce 排序，仍受节点冷却、可用性及链上状态约束。同一轮已排队或在途时复用该轮。 |
| POST /transactions/{hash}/replacement | rawTransaction | 用路径指定待替换的当前版本。新签名交易通过校验，且 chainId、sender、nonce 相同后，在原任务下登记新 Hash，返回 202。重复提交同一已登记替换版本返回 200。原 Hash 继续接受链上监控。 |
| GET /accounts/{address}/queue?chainId=… | 地址、目标链 | 按外层 Nonce 升序返回未关闭任务、当前版本、排队原因和下次调度时间。队列只列已接收的任务。 |
| GET /accounts/{address}/nonce?chainId=… | 地址、目标链，可选 blockTag（latest 或 pending） | 返回最新链上 chainNonce 及结合队列的推荐 nonce。指定 pending 时还返回同一节点的 pendingNonce。均为十进制字符串。所有节点读取失败返回 503 NONCE_VIEW_UNAVAILABLE。 |

**冲突与错误。** 原文无法解码、签名无效、链未启用或整笔交易违反协议规则时，提交返回 4xx 和具体原因，校验失败的请求不占用 Nonce。已有 Hash 的幂等查询优先于依赖当前链状态的预检，先校验调用方访问权限。活动 EVM 任务的唯一键为 (chainId, sender, nonce)，签名交易以 (chainId, txHash) 去重。Hash 幂等检查与新任务插入在同一事务内完成，唯一键冲突时重新读取已提交的任务作判定。并发 replacement 使用当前版本的条件更新；对过时版本提交不同的新 Hash、在核查 Nonce 消耗者或已关闭时尝试替换，以及对不可调度版本执行 force，均返回 409 和原因。已关闭任务保留提交历史及终止结果。

**提交响应示例**（标识符省略中间字符）：

~~~json
{"taskId":"task_42","network":"1","txHash":"0x…","state":"QUEUED"}
~~~

202 表示服务已持久化任务。调用方可用返回的 Hash 查询进度；自动重播、查回执和故障恢复无需调用 retry 接口。

## EVM Nonce 查询与任务排序

每次 `GET /accounts/{address}/nonce?chainId=…` 随机选择一个健康 RPC 读取 `eth_getTransactionCount(address,"latest")`，失败后换下一家。接口从链上值开始，跳过同链同发送方所有调用方的连续活动任务，返回第一个空位。可选 `blockTag=pending` 会在同一节点再读取 pending，从两者较大值开始。只有 latest 能触发已占用 Nonce 的链上核查。[Ethereum JSON-RPC](https://ethereum.org/developers/docs/apis/json-rpc/#eth_gettransactioncount)

例如链上返回 20，本地活动任务占用 20、21、23，默认响应含 `"chainNonce":"20","nonce":"22"`。签名交易成功入队时才占用槽位；查询本身不预留，也不保存链上读数或推荐值。7702 代付时对代付地址调用此接口，为外层签名选择 Nonce；用户授权使用用户自己的账户 Nonce。

~~~mermaid
flowchart TD
    Read["GET nonce：随机 RPC 查 latest，再查活动队列"] --> Sign["选取外层发送账户的 Nonce，签名"]
    Sign --> Submit["POST /transactions：原子登记任务"]
    Submit -->|相同 Hash| Existing["返回已有任务"]
    Submit -->|不同 Hash 且任务键冲突| Conflict["409 NONCE_CONFLICT，本次提交不入队"]
    Submit -->|新任务| Queue["按链、外层发送方和 Nonce 排队"]
    Conflict -->|独立付款| Read
    Conflict -->|确需替换原任务| Replace["有权限的调用方显式提交 replacement"]
    Replace --> Queue
~~~

数据库保存签名原文及任务键 (chainId, sender, nonce)，用于并发判重、排序和重启恢复。若两个服务都在第一笔交易入队前查询，它们可能拿到同一个推荐值。不同交易使用同一槽位时，只有一次普通提交能创建任务。另一笔若是独立付款，由签名方再次查询推荐值后重新签名。只有同一任务的有意替换才用 replacement。

发现外层账户的链上 Nonce 已越过本地任务时，逐笔核查候选 Hash。自己的版本入块则正常关闭；证实外部消耗则通知原提交方并重新调度剩余任务；证据不足则暂停该任务重播并继续核查。查询发现低于 latest 的排队或跟踪任务，会在同一数据库事务内转入 RESOLVING_NONCE 并唤醒工作实例。工作实例核实结果后通知上游。已关闭任务保留 Hash 历史；若链上槽位仍可用，它可以被新任务使用。过期任务的原文仍可能入块，重付前须核对业务结果。

## RPC 节点管理

每个端点归属于一个稳定的 providerId 和一条链。同一供应商的多个域名、密钥或端点只占一路。每个 EVM 网络使用独立的节点池，可组合 PublicNode 和自购端点，所有端点须能广播普通 EVM 交易及该链配置启用的交易类型。除采用 Cancun 规则的 HyperEVM（999）外，准入另需通过 EIP-7702 Type 4 广播探针。配置了 EntryPoint 的链还须通过相应的 ERC-4337 外层交易探针。4337 外层交易调用目标链的 EntryPoint 合约，经 eth_sendRawTransaction 提交。面向钱包的 eth_sendUserOperation 由独立 Bundler 提供。

| 方法与路径 | 行为 |
|---|---|
| POST /rpc-providers | 管理员提交 providerId、chainId、端点和凭证配置。验证实际 chainId、鉴权、查询与广播权限、普通交易和该链启用的 Type 4 广播能力，以及对已配置 EntryPoint 的 4337 外层交易广播权限。全部通过才登记启用并返回 201。失败或能力无法证实时返回具体检查项与原因。 |
| GET /rpc-providers?chainId=… | 返回供应商和端点标识、启停状态、最近高度及观测时间、读和广播健康状况、失败计数、冷却截止时间与最近错误。凭证与 URL 中的密钥须脱敏。 |
| PATCH /rpc-providers/{id} | 以端点记录 ID 修改配置或启停。端点、凭证或链变更时先重新准入，检查通过后原子切换；失败时原配置继续生效。停用后停止分配新请求。 |

准入须有目标端点和对应凭证的能力证据。例如可使用运维提供的受控已签名测试交易，验证对应写接口接受该类型；费用及链上副作用须在准入测试安排中明确。对 4337，按目标链登记 Bundler 使用的 EntryPoint 地址和版本，核对合约部署，并验证执行端点允许发送有效的、调用该 EntryPoint 的已签名 bundle。查高度成功、能读取历史 Type 4 交易或收到通用参数错误，均不足以证明该端点有 Type 4 广播权限。

Solana 按集群单独建池。准入时用 getGenesisHash 核对集群，验证凭证、sendTransaction 写权限以及交易状态、历史和区块高度查询能力。以 getBlockHeight 和节点进度做读侧健康检查。18 条链共用下述供应商去重、广播熔断、半开试发及恢复规则。

### PublicNode 默认节点

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

公开版本只提供 PublicNode 的节点地址。Monad、Plasma 和 TON 在 PublicNode 官方目录暂无主网端点。生产部署若需每链三路，已提供预设的链再登记两家独立供应商，其余三条各登记三家。付费地址和凭证通过私有管理接口登记，保存于部署环境的数据库。

**启用。** 向 `POST /rpc-providers` 提交 `providerId: "publicnode"`、目标 `network` 和该链的签名准入探针即可，URL 自动选择。例如以太坊请求体形如 `{"providerId":"publicnode","network":"1","evmProbes":["0x…","0x…","0x…"]}`。首次通过全部检查后进入调度池；重复提交返回原记录。运行中的节点和启停状态可通过 `GET /rpc-providers` 查询。

**启动时自动准入。** 在相应网络配置中将 `publicNodeProbeFile` 指向 JSON 文件，路径相对于配置文件。示例配置的默认路径为 `probes/<网络键>.json`。EVM 文件包含 `evmProbes`，Solana 文件包含 `solanaProbe`，Bitcoin、TRON、Aptos 和 Sui 文件包含 `nativeProbe`，格式与上述准入 API 相同。首次启动使用文件中已获授权的签名原文做真实准入；失败原因写入日志，可更新探针后通过 API 再次准入。重启复用 SQLite 中的记录及人工停用状态。

生产管理员核对付费供应商对具体广播地址的说明后，可在 POST 或 PATCH 中设置 `mevProtected: true`。更换供应商、网络、URL 或凭证时需重新声明该标记。PublicNode 的标记由内置官方目录决定。

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

## EVM 交易生命周期

**结束条件是候选 Hash 已出现在观测时的规范区块中。** 保存交易 Hash、区块号、区块 Hash、交易索引和观测时间。可通过区块交易列表或与规范区块核对过的回执建立入块证据；只有 RPC 接收或 pending 交易对象时继续跟踪。入块即结束广播任务，关闭后不追踪区块重组，也不承诺链级最终性。

任务汇总一个业务提交及其替换版本；签名交易记录每个 Hash；广播记录保存某 Hash 向一家供应商的一次请求。接口分别返回 taskState、taskOutcome、executionStatus。入块结果为 INCLUDED。回执 status 为 1 时 executionStatus 为 SUCCESS，0 为 FAILED，暂缺回执则为 UNKNOWN。查询可在取得同一区块的回执后补充执行结果。回执失败的交易同样已入块。[EIP-658 回执状态](https://eips.ethereum.org/EIPS/eip-658)

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
    Q -->|候选已在块内| I["CLOSED / INCLUDED"]
    T -->|候选已在块内| I
    R -->|本任务任一候选在块内| I
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
| 任意未关闭状态 | 在当前规范区块核实任务内某个 Hash | CLOSED / INCLUDED，记录 winningHash 及独立执行结果，其他候选记 REPLACED。通知上游并更新账户队列。 |
| RESOLVING_NONCE | 定位到任务外交易消耗该 Nonce（含已生效的 7702 授权） | CLOSED / REPLACED_EXTERNALLY。返回错误 NONCE_CONSUMED_EXTERNALLY、受影响的本地 Nonce 和 Hash、消耗者及区块，停止全部候选。通知原提交方并重排队列。 |
| 未关闭状态 | 所有候选永久无效且证据充分 / 配置的追踪期限到达 | 分别以 CLOSED / REJECTED 或 CLOSED / EXPIRED 结束。后者保留最后观测，已发原文仍可能上链。 |

已接收任务的外部覆盖通过 GET 任务结果和异步事件报告。核查期间查询可见 RESOLVING_NONCE；单个 RPC 拒绝、空回执、链上 Nonce 增长都不能单独证明“外部覆盖”。已绑定 Hash 的本地任务历史可追溯；新的签名交易不得复用被链上消耗的槽位。

**重组与供应商分歧。** 每轮并行核查可读端点。发现候选入块或外部消耗时，在回执查询、7702 诊断和历史查询完成后，重新按高度读取见证节点的规范块，并比对其他可读节点在该高度的区块 Hash。任一节点给出不同的块、候选赢家或明确相反的执行结果，任务进入 `RESOLVING_NONCE / CHAIN_VIEW_CONFLICT`，暂停重播，下一轮复核。落后节点尚无该块或暂时读不到时，可依据一家完成核查的供应商关闭。一个旧候选的孤块回执不会挡住新候选的核查。任务保存获胜 Hash、区块及观测时间，只向上游报告一次逻辑关闭事件。关闭之后发生重组，查询仍返回已保存的入块快照；依赖最终性的结算方需自行等待目标链的最终性条件。[EIP-1898 对跨 RPC 读取一致性的说明](https://eips.ethereum.org/EIPS/eip-1898)

### 签名交易与节点请求

| 当前 Hash 状态 | 条件 | 下一状态与动作 |
|---|---|---|
| QUEUED | 被选为当前版本且可发送 | BROADCASTING。持久化轮次再并发发送；未满足条件继续排队。 |
| BROADCASTING | 一家接收或明确已知该 Hash | PENDING，继续收集同轮其他结果。 |
| BROADCASTING | 至少一路结果不明且无明确接收 | SUBMISSION_UNKNOWN。按本地 Hash 核查，避免在超时后误报失败。 |
| BROADCASTING | 全部明确拒绝 | 可恢复错误回 QUEUED，记录退避或阻塞原因；提示 Nonce 过低则触发账户对账；永久原文错误记 REJECTED。 |
| SUBMISSION_UNKNOWN | 查到 pending / 暂不可见且 Nonce 可用 | 前者转 PENDING；后者当前版本按策略回 BROADCASTING，旧版本保持观察。 |
| PENDING | 仍未入块 | 保持 PENDING。可重播同一原文；费用不足时通知签名方提供 replacement。新一轮超时保留先前接收证据。 |
| 任意仍可能入块的候选 | 本 Hash 入块 / 同任务其他 Hash 入块 / 外部消耗得到核实 | 分别落为 INCLUDED / REPLACED / REPLACED_EXTERNALLY。任务同时关闭。 |
| 任意仍在追踪的候选 | 任务到期 | 停止主动调度和监控，保留最后观测。 |

每条节点请求从 IN_FLIGHT 转为 ACCEPTED、ALREADY_KNOWN、REJECTED 或 UNKNOWN。记录轮次、端点、供应商、时间、延迟、错误分类及脱敏原始错误。迟到响应只补充节点记录，已关闭任务保持原结果。

按任一候选 Hash 查询，返回 taskId、taskState、taskOutcome、chainId、sender、nonce、queriedHash、activeHash、winningHash、consumingHash、attempts 及阻塞原因。入块附 completionBasis=BLOCK_INCLUSION、区块证据、executionStatus、receipt 和逐条 authorizations。外部覆盖时返回错误及链上证据。实时账户 Nonce 可另行查询。

### ERC-4337 广播与查询

| 调用方 → 服务 | 接口与标识 | 职责 |
|---|---|---|
| 钱包 → Bundler | eth_supportedEntryPoints、eth_sendUserOperation；返回 userOpHash | 钱包选择该链上 Bundler 支持的 EntryPoint，提交签名的 UserOperation。Bundler 校验、模拟并打包。 |
| Bundler → Broadcaster | POST /transactions，传外层 rawTransaction；返回 taskId、txHash | Bundler 可先查询**外层签名账户**的 Nonce，再签署调用 EntryPoint.handleOps 的交易。沿用同一套最多三家并发、自动重播、显式替换和入块即关闭规则。携带新 7702 授权的 bundle 使用 Type 4 并执行逐条授权诊断。 |
| Bundler → Broadcaster | GET /transactions/{hash} 或入块通知 | Bundler 保存 userOpHash 到外层任务的关联，按 winningHash 定位实际入块版本。若发生替换或重新打包，以实际入块交易中可核实的操作为准。 |
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
2. 代付服务构造并签署 R 的外层交易，保存业务请求与已签名原文、Hash 的关联，调用 POST /transactions。Broadcaster 在 R 的队列中按外层 Nonce 调度。
3. 提交响应超时则重交相同原文或查询原 Hash。另一笔付款误用相同 Nonce 时，普通提交返回 409；只有需要替换已接收任务时才提交同 Nonce 的签名替换版本。

例如，代付签名方为 R 的两笔付款选定 `100` 和 `101`。如果两笔都属于 A，而 D 对 A 采用顺序操作编号，签名流程还要分别协调 `7` 和 `8`，并保持执行顺序。A 用外部钱包普通转账只会推进 A 的链上账户 Nonce；已签操作仍可能因余额变化而执行失败。实际操作编号与签名字段以所部署的 D 为准。[EIP-7702 授权处理](https://eips.ethereum.org/EIPS/eip-7702#behavior) · [委托持久性](https://eips.ethereum.org/EIPS/eip-7702#persistence-of-code-delegation) · [签名与防重放](https://eips.ethereum.org/EIPS/eip-7702#security-considerations)

### EIP-7702 诊断

1. **提交时**区分整笔无效和个别授权可能被跳过。空授权列表等整笔格式错误直接返回；逐条预检保留索引、委托目标、可恢复的授权者地址、签名 Nonce、原因及所用区块。按协议顺序计算外层发送者 Nonce 的自增，以及同一列表里前序有效授权的影响。
2. **执行时**分别报告 RPC 广播、外层回执和每条授权结果。使用交易在区块中的实际位置重放或等价证据，才能确定该条 APPLIED 或 SKIPPED 及原因。只有预检、标准回执或区块末账户代码时，对无法证明的条目返回 UNKNOWN，并保留预测及证据来源。
3. **记录结果时**绑定观测到的区块 Hash。外层 status=1 时部分授权仍可能被跳过；外层执行回退时已应用的委托仍可保留。[EIP-7702 协议](https://eips.ethereum.org/EIPS/eip-7702#behavior)

## Solana 代付交易

GM 的代付地址是 fee payer，用户授权转账。上游先确定集群、付款指令、代付地址和 recent blockhash，用户与 GM 的签名服务对**同一份 message** 签名，再提交完整序列化交易。Broadcaster 验证签名并广播。第一份签名属于 fee payer，也是 RPC 查询所用的交易 ID（下文记为 signature）。同一 GM 地址可同时代付多笔交易，各笔独立调度，提交顺序不保证链上执行顺序。本节适用于使用 recent blockhash 的交易；durable nonce 交易不适用这套过期规则。[Solana 交易结构](https://solana.com/docs/core/transactions/transaction-structure)

### 提交与幂等

| 方法与路径 | 输入 | 行为 |
|---|---|---|
| POST /solana/transactions | cluster、paymentId、signedTransaction（base64）、本次 getLatestBlockhash 返回的 context.slot 和 lastValidBlockHeight | 解析 message 中的 recentBlockhash，校验适用的交易格式、GM fee payer、用户及全部必需签名。缺签、签名无效或采用 durable nonce 时返回具体 4xx 原因；成功持久化返回 202、taskId 和 signature，直接进入待广播状态。 |
| GET /solana/transactions/{signature}?cluster=… | 任一版本的第一份签名 | 返回同一支付任务、各版本签名和状态、blockhash、有效高度、逐供应商结果、入块 slot 和执行错误（如有）。 |
| POST /solana/transactions/{signature}/resign | cluster、重新双签的完整原文和新 blockhash 的上下文及有效高度 | 仅当所指旧版本为任务当前版本且已进入 RESIGN_REQUIRED 时接收。核对同一付款、代付地址及指令与原版本一致，只更新 blockhash 和签名；在同一事务中登记新版本并调度。过时版本或旧交易结果待核实返回 409。 |

paymentId 由代付服务为同一笔业务付款稳定生成。按调用方、集群和 paymentId 唯一创建任务，按集群和 signature 去重。重交已登记的相同签名返回 200 及原任务；同一付款经普通入口提交另一份交易返回 409。重新签名接口重试已登记版本也幂等。恢复时保留所有版本，以旧 signature 查询仍能看到最终结果。

### 广播、过期与重新签名

| 当前状态 | 依据 | 下一步 |
|---|---|---|
| READY | 交易已全签并持久化，blockhash 仍有效 | 立即进入 BROADCASTING。对最多三家独立供应商并发调用 sendTransaction，发送相同的 base64 原文。两家或一家可用就立即发；零家可用时保持 READY 并继续监测有效期。 |
| BROADCASTING | 一家返回与本地一致的 signature | 进入 TRACKING，收集其他在途响应。RPC 接收只证明已提交；按 signature 查链上结果。 |
| BROADCASTING / TRACKING | 超时、断连、单节点报 BlockhashNotFound，或只观察到 processed | 继续按原 signature 核查。有效期内按需重播相同字节；拒绝原因按交易或端点归类。单个落后节点的响应不能触发重签。 |
| 任意未结束状态 | 核实原 signature 在该集群达到 confirmed 或 finalized | 以 INCLUDED 关闭，保存实际 signature 和 slot。按 err 是否为空分别返回 SUCCESS 或 FAILED；执行失败也属于已入块，停止重播及过期重签。 |
| READY / BROADCASTING / TRACKING | 同集群健康节点的 getBlockHeight 已**严格大于**该 blockhash 的 lastValidBlockHeight | 进入 CHECKING_EXPIRY，停止重播旧原文，核查旧签名是否已在有效期内入块。比较的是 block height；context.slot 只用于约束 RPC 数据新鲜度。 |
| CHECKING_EXPIRY | 历史记录查到旧签名已入块 / 结果有分歧或历史覆盖不足 | 已入块按 INCLUDED 关闭；证据不足保持核查，可见 processed 时继续追踪其归属。单次查无记录或墙钟超时均不足以要求重新付款。 |
| CHECKING_EXPIRY | 已核实旧 blockhash 失效，且有覆盖有效窗口的规范链历史证据表明旧签名未入块 | 置 RESIGN_REQUIRED，通过 outbox 向上游发送 solana.resign_required，附 taskId、paymentId、旧 signature 和过期依据。 |
| RESIGN_REQUIRED | 上游获取新 recent blockhash，用户与 GM 对更新后的同一 message 全部重新签名，并调用 resign | 新版本从 READY 开始，产生新的 signature，复用原 taskId 和 paymentId。等签名期间不发起广播。 |

签名方使用 getLatestBlockhash 一次取得 blockhash 及其 lastValidBlockHeight，建议使用 confirmed，广播预检采用一致的 commitment，并传 minContextSlot 避免落后节点误判。有效高度是签名原文之外的元数据；应核对消息中的 hash 与上游取得的配对记录。授权重签前，用健康节点核实同一旧 hash 在 finalized 视图中已失效、该视图已越过有效高度，再用 getSignatureStatuses（searchTransactionHistory=true）及具备历史覆盖的节点核查旧签名。无法确认旧交易结果时继续查询并对外报告原因。进程重启或通知重投都沿用已持久化的支付任务和签名版本。[获取 blockhash](https://solana.com/docs/rpc/http/getlatestblockhash) · [发送交易](https://solana.com/docs/rpc/http/sendtransaction) · [查询签名](https://solana.com/docs/rpc/http/getsignaturestatuses) · [交易确认与过期](https://solana.com/docs/advanced/confirmation)

### 分叉与不完整的历史

服务在接单时，或首次观测到有效 blockhash 时，向 RPC 查询签名上下文所指的区块。只有该块的实际 Hash 与签名原文一致，才持久化其创建 slot。过期核查沿 finalized 区块的 parentSlot 和 previousBlockhash 回溯并检查每个区块的签名列表，进度可跨进程重启恢复。

| 观测 | 处理 |
|---|---|
| 核查期间 confirmed 视图再次证明原 hash 有效 | 恢复跟踪，按原签名重播；若其他节点已有过期的历史证据，则先处理分歧。 |
| 已见证的创建块后来成为孤块 | 最终链高度越过有效期、旧 hash 失效，且完整父链回溯覆盖创建 slot 后仍查无签名，可以要求重签。没有可靠的创建 slot 时，查到签名对应的原 blockhash 也能界定有效窗口。 |
| 节点仍返回 processed | 已验证的最终链历史覆盖该 slot 时可排除已丢弃分叉上的观测。processed 高于核查起点时，继续等待新 finalized 视图。 |
| 历史缺块、父 Hash 不符或创建位置无法验证 | 保持 CHECKING_EXPIRY，保留可用进度，继续尝试其他节点。调用方提供的 slot、高度和一次空查询都不能单独授权重签。 |
| 节点在 confirmed 层给出冲突的入块位置或执行结果 | 暂停关闭，等待一致视图或更强的 finalized 证据。 |
| 节点恢复后发现调用方在断网接单时给出的有效高度过大 | 用新鲜的 finalized blockhash 有效窗口识别矛盾，再以完整历史核查旧签名。 |

## 故障恢复与上游事件

接收事务写入任务、签名原文和调度信息后再响应。工作实例以租约领取到期任务；重启后接手失效租约，先核查全部候选 Hash，再决定按入块关闭、等待或重播。实际 RPC 调用允许重复，数据库对任务、候选和事件去重。SQLite 保留恢复所需的原文、状态与证据。

状态变更和待发事件通过同一事务写入 outbox。消息队列或 Webhook 至少一次投递，失败可重试，消费方按 eventId 去重并按任务版本处理乱序。transaction.submitted 表示已持久化，入块时 transaction.included 和 transaction.closed 携带入块证据及独立执行状态，其他终止路径的 closed 携带对应原因，transaction.replacement_required 通知签名方加价。任务被外部覆盖时发送 transaction.nonce_conflict 及 transaction.closed，携带 taskId、本地 Hash、链、账户、Nonce、consumingHash、区块和错误码。鉴权按接收方隔离。通知投递失败只重试通知。

## 首版验收示例

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
| Solana 旧 blockhash 已过期，历史查询随后查到原签名已入块 | 关闭原任务并返回执行结果，即使支付服务重试也不触发重签。 |
| Solana 旧签名确认未入块且 blockhash 已失效，上游只提供新的 GM 签名 | 缺用户的新签名时拒绝；双方对新消息签齐后才登记新版本。重复提交同一新签名只产生一个版本。 |

## 原生链广播接口

适用于 `bitcoin`、`tron`、`aptos`、`sui`、`ton`。请求须使用业务 Bearer Token。配置、准入和业务路径使用相同的网络键。

| 方法与路径 | 行为 |
|---|---|
| `POST /networks/{network}/transactions` | JSON 必填 `rawTransaction`。Sui 还须提供 `signatures` 字符串数组。首次创建返回 202，包含 `taskId`、`network`、`txHash` 和 `state`。同一调用方重交该标识返回 200 及原任务。 |
| `GET /networks/{network}/transactions/{txHash}` | 按提交时返回的标识查询。响应包含任务状态、每次供应商广播的结果、入块证据及执行状态。其他调用方查询该任务返回 404。 |
| `POST /rpc-providers` | 管理员提供 `network`、`providerId`、`url` 和与上述 POST 请求体同格式的 `nativeProbe`。服务检查网络身份、读接口，并发送探针验证写权限。 |

| 网络 | 供应商 URL 与广播接口 | 查询标识与证据 |
|---|---|---|
| Bitcoin | Bitcoin Core JSON-RPC 2.0 网关，须启用并同步 txindex。发送用 `sendrawtransaction`。如节点使用 Basic Auth，可由私有网关注入凭证。 | 本地计算 txid，隔离见证交易同样按 txid 查询。`getrawtransaction` 与 `getblockheader` 核对活动链区块及确认数。 |
| TRON | 配置可访问 Wallet 和 WalletSolidity 的根 URL。交易按 protobuf 编码为 hex，通过 `/wallet/broadcasthex` 发送。 | txID 为原始交易数据的 SHA-256。读取固化视图的交易回执，并核对指定块中确有同一 txID。 |
| Aptos | 配置完整的 fullnode REST API 根路径，通常以 `/v1` 结尾。将 Base64 解码后的原始 BCS 字节发送至 `/transactions`。 | 对原始签名交易按 Aptos 用户交易的域前缀计算哈希。节点校验签名和链 ID。按哈希查询，pending 继续跟踪，已提交的 user_transaction 返回账本版本及 success。 |
| Sui | 配置 gRPC v2 的 HTTP(S) 主机 URL。使用 `ExecuteTransaction` 传递 TransactionData 和全部签名，多个签名按原样发送。 | 根据 TransactionData 原始字节计算 digest。`GetTransaction` 同时返回相同 digest、checkpoint 及 effects.status 后关闭。 |
| TON | 配置同时提供 TON Center v2 写接口和 v3 索引的网关根 URL。调用 `/api/v2/sendBoc` 提交单根外部入站消息。 | 返回小写十六进制的规范化消息哈希。使用该哈希查询 v3 索引，核对返回的入站消息规范化哈希及块位置，返回接收方账户交易哈希和逻辑时间。 |

例如，Sui 请求体形如 `{"rawTransaction":"<base64 TransactionData>","signatures":["<base64 UserSignature>"]}`。另外四条链只传 `rawTransaction`。签名服务负责 Bitcoin 的输入选择、TRON 的交易有效期、Aptos 的账户序号、Sui 的对象引用以及 TON 钱包的重放保护。

原生链任务从 `QUEUED` 开始调度。提交给节点后进入 `TRACKING`，复用三供应商策略、SQLite 重启恢复和 Webhook。读取不完整或节点对入块位置、执行结果存在分歧时继续核查。证据一致时进入 `CLOSED / INCLUDED`，单独记录执行成功或失败。超过 `trackingHours` 仍无足够证据则以 `CLOSED / EXPIRED` 结束本地跟踪；需要重付时先核对原交易的链上结果。
