# 本地 Xray-core 补丁

基线：上游 [v26.10.10](https://github.com/XTLS/Xray-core/releases/tag/v26.10.10)，
提交 `701af60772cda123492f86e383e1fdba066614a9`。
XrayR 的 `go.mod` 使用本地版本标识 `v1.261010.0`，并继续通过 `replace`
引用本目录；源码固定来自上述官方标签，不使用 main 分支或未合入的 PR。
本机与项目工具链升级到 Go 1.27.1，GitHub Actions 固定使用 `1.27.x`，
Docker 构建镜像使用 `golang:1.27-alpine`。

## 使用上游实现

- 纳入 v26.10.10 的全部上游增量：Lua DNS/路由脚本、TLS 内置根证书
  与 `useSystemCA`、SOCKS UDP Associate 入站计数、Mux 并发和 UDP 地址修复、
  DNS pubsub 容量修复、gRPC 重连、XDNS、MASQUE/WARP、WireGuard 和 TUN 更新。
  XrayR 自定义分流器同步上游的路由错误日志，保留本地统计和限速包装。
- GeoData：使用上游 `common/geodata` 的延迟加载、紧凑匹配器和缓存；
  删除旧版 `app/router/geosite_compact.go` 及其解析/匹配适配补丁。
- XHTTP：保留上游 `GetNormalized...` 返回指针、`atomic.Bool` 关闭状态和
  `WaitReadCloser` 原子读写实现，不恢复旧版实现。
- Hysteria：使用上游基于目标地址和 `MemoryStreamConfig` 的结构体缓存键，
  保留新版 QUIC、ChromeParrot、FinalMask 及拥塞控制逻辑。
- 不恢复上游已经删除的配置和不安全功能，包括 `allowInsecure` JSON 参数、
  Shadowsocks `none/plain`。

## 继续保留的补丁

- `infra/conf/xray.go`：继续支持旧版 freedom `settings.domainStrategy`，
  静默映射到新版 `streamSettings.sockopt.domainStrategy`，不输出该字段的弃用警告；
  XrayR 自动生成的出站及自定义出站配置均无需迁移。保留新版拨号实现、
  新格式支持及其他配置校验/警告，不回退整个出站模块。
- `common/net/destination.go`：`RawNetAddr` 对空地址返回 nil，避免 UDP 空指针崩溃。
- `app/proxyman`、`core/xray.go`：关闭入站活动连接、UDP 清理任务和出站池；
  动态处理器添加失败时回滚，关闭/移除处理器时释放其资源。
- `app/dns`：关闭缓存、DNS 客户端、周期清理任务及 DoH/DoQ 连接。
  新版 DNS Lua 状态池关闭与这些清理合并执行，不覆盖为上游仅关闭脚本的实现。
- `common/buf`、`common/mux`、`common/signal/pubsub`：复用挂起的超时读取，
  优先使用原生超时；超时包装器透传中断/关闭，避免黑洞超时后读取仍挂起；
  释放 Mux 清理任务和订阅状态。
- `transport/internet`：登记可重复调用的全局传输清理器，保护 DNS/出站管理器引用；
  关闭 gRPC、XHTTP、Hysteria 缓存。
- XHTTP：关闭 XMUX 客户端及 HTTP transport；使用可显式关闭的 HTTP/1 上传连接池。
  XMUX 配置使用指针，避免复制含锁的 protobuf 对象。
- Hysteria：未指定 QUIC keepalive 时保留 10 秒默认值；定期清理失效/空闲客户端，
  关闭 QUIC transport、UDP socket 和对应 TLS 证书监视器。
  周期任务在释放缓存锁后启动，避免首次同步清理触发死锁。
  纳入上游的实例状态关联与 `forced` 关闭标志；关闭/移除的客户端不可重新拨号。
  清理先复制缓存快照，释放缓存锁后检查实例状态和关闭客户端；删除前校验
  缓存仍指向同一个客户端，避免阻塞其他节点或删除并发创建的新客户端。
  保留可停止的 `task.Periodic` 与全局清理器，不恢复上游无法停止的 ticker。
- TLS/REALITY/ECH/OCSP：停止证书监视器；同步证书更新；复用并关闭 keylog 文件；
  限制 OCSP 请求时间、响应大小，清理 ECH 缓存。

XrayR 自身的生命周期和热重载补丁保持不变；分流器按新版 stats 和
GeoData 排除规则接口适配。Hysteria2 入站继续显式设置 `h3` ALPN。

## 兼容性与验证

服务端文件证书配置无需因本次升级修改，`RejectUnknownSni` 省略时仍为 false。
`service/controller/core_compat_test.go` 验证文件证书路径、不同域名的 SNI 握手、
Vision 用户账号及 Hysteria2 ALPN/认证参数，并通过实际 Hysteria2 TCP 转发
检查认证、XrayR 分流器和数据完整性。
此测试中的跳过证书验证由 Go TLS 测试客户端执行，不代表新版 Xray 客户端
接受 `allowInsecure`；本次没有修改客户端。

`service/controller/outbound_compat_test.go` 验证自动生成出站及旧版 JSON 的
各项域名解析策略、默认值、新格式和旧字段优先级；检查旧字段不产生弃用警告，
且无效策略仍被拒绝。
此兼容补丁复测三轮通过，Hysteria2、证书和用户配置回归通过；XrayR 全包
编译、controller 静态检查及 Linux amd64 构建通过。内核配置测试排除
GeoData 用例后通过（缺少 `resources/geoip.dat`、`geosite.dat`）。
完整 `go vet ./infra/conf` 仍报告上游原有的 legacy reverse 返回后的不可达代码；
核对上游原版后保留该实现，排除 unreachable 检查的其余静态检查通过。

已用本机 Go 1.27.1 复测 XrayR 编译和功能、Windows/Linux amd64 构建，
以及 DNS、入/出站、Mux、超时读取、Hysteria、TLS 和 XHTTP 本机回归测试。
上游 VLESS TLS/XTLS Vision 的实际数据传输测试通过。
Windows 上游 Unix socket 测试失败，公网 DNS 测试未作为离线验收依据；
本次未连接生产面板、未部署或重启服务，也未进行生产环境客户端端到端验收。

临时上游工作树、升级前备份及验证二进制放在 XrayR 仓库的 `.tmp-xray/`，
该目录已忽略，不进入提交。

## v26.10.10 同步验证

- 通过旧基线、本地源码、新标签的三方合并保留现有补丁及未提交修复；
  与上游无重叠的原文件保持不变，新增上游文件和未打补丁的更新文件核对一致
  （统一文本换行，不要求原文件缺失的末尾换行）。
- XrayR 的流量、限速、用户增删/304 重试、异常日志、旧 freedom 配置、
  文件证书 SNI、VLESS Vision 实际传输及计数、Hysteria2 实际传输回归三轮通过。
- 新版 Lua DNS/路由、配置解析及 GeoData Lua 绑定离线用例三轮通过；
  DNS 关闭同时释放脚本池和上游客户端、Hysteria 已关闭实例与缓存锁竞争、
  TLS 根证书策略新增回归通过。Lua、TLS、Mux、QUIC、MASQUE 和 WireGuard
  的本机单元测试通过。
- 全仓编译检查、关键包静态检查及 Windows/Linux amd64 构建通过；
  保持 Go 1.27.1 和 Actions 1.27.x。
- 本地没有 `resources/geoip.dat`、`geosite.dat`，依赖完整数据文件的用例
  因缺少文件失败，未作为通过项；公网 DNS、外部面板及生产部署不作为本机验收。
- 此次仅本地同步，不提交、推送、打 tag 或重启生产服务。
