# 本地 Xray-core 补丁

基线：上游 [v26.9.30](https://github.com/XTLS/Xray-core/releases/tag/v26.9.30)，
提交 `b26a91de4f3294e26a0ad0a970b81a386a41f789`。
XrayR 的 `go.mod` 使用 `v1.260930.0`，并继续通过 `replace` 引用本目录。
本机与项目工具链升级到 Go 1.27.1，GitHub Actions 固定使用 `1.27.x`，
Docker 构建镜像使用 `golang:1.27-alpine`。

## 使用上游实现

- GeoData：使用上游 `common/geodata` 的延迟加载、紧凑匹配器和缓存；
  删除旧版 `app/router/geosite_compact.go` 及其解析/匹配适配补丁。
- XHTTP：保留上游 `GetNormalized...` 返回指针、`atomic.Bool` 关闭状态和
  `WaitReadCloser` 原子读写实现，不恢复旧版实现。
- Hysteria：使用上游基于目标地址和 `MemoryStreamConfig` 的结构体缓存键，
  保留新版 QUIC、ChromeParrot、FinalMask 及拥塞控制逻辑。
- 不恢复上游已经删除的配置和不安全功能，包括 `allowInsecure` JSON 参数、
  Shadowsocks `none/plain`。

## 继续保留的补丁

- `common/net/destination.go`：`RawNetAddr` 对空地址返回 nil，避免 UDP 空指针崩溃。
- `app/proxyman`、`core/xray.go`：关闭入站活动连接、UDP 清理任务和出站池；
  动态处理器添加失败时回滚，关闭/移除处理器时释放其资源。
- `app/dns`：关闭缓存、DNS 客户端、周期清理任务及 DoH/DoQ 连接。
- `common/buf`、`common/mux`、`common/signal/pubsub`：复用挂起的超时读取，
  优先使用原生超时；释放 Mux 清理任务和订阅状态。
- `transport/internet`：登记可重复调用的全局传输清理器，保护 DNS/出站管理器引用；
  关闭 gRPC、XHTTP、Hysteria 缓存。
- XHTTP：关闭 XMUX 客户端及 HTTP transport；使用可显式关闭的 HTTP/1 上传连接池。
  XMUX 配置使用指针，避免复制含锁的 protobuf 对象。
- Hysteria：未指定 QUIC keepalive 时保留 10 秒默认值；定期清理失效/空闲客户端，
  关闭 QUIC transport、UDP socket 和对应 TLS 证书监视器。
  周期任务在释放缓存锁后启动，避免首次同步清理触发死锁。
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

已用本机 Go 1.27.1 复测 XrayR 编译和功能、Windows/Linux amd64 构建，
以及 DNS、入/出站、Mux、超时读取、Hysteria、TLS 和 XHTTP 本机回归测试。
上游 VLESS TLS/XTLS Vision 的实际数据传输测试通过。
Windows 上游 Unix socket 测试失败，公网 DNS 测试未作为离线验收依据；
本次未连接生产面板、未部署或重启服务，也未进行生产环境客户端端到端验收。

临时上游工作树、升级前备份及验证二进制放在 XrayR 仓库的 `.tmp-xray/`，
该目录已忽略，不进入提交。
