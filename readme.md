# AnyTLS

一个试图缓解 嵌套的TLS握手指纹(TLS in TLS) 问题的代理协议。`anytls-go` 是该协议的参考实现。

- 灵活的分包和填充策略
- 连接复用，降低代理延迟
- 简洁的配置

## 协议与设计

[用户常见问题](./docs/faq.md)

[协议文档](./docs/protocol.md)

[URI 格式](./docs/uri_scheme.md)

## 示例项目

> 📌 **项目定位**：本项目是 AnyTLS 协议的**参考实现**，核心目标是**演示协议细节、交互流程与边界场景**，而非提供功能完备的生产级客户端。日常使用推荐 [第三方兼容软件](#第三方兼容软件)。

> ⚠️ 示例服务器和客户端默认采用不安全的配置，该配置假设您不会遭遇 TLS 中间人攻击；否则，您的通信内容可能会被中间人截获。

### 示例服务器

```
./anytls-server -l 0.0.0.0:8443 -p 密码
```

`0.0.0.0:8443` 为服务器监听的地址和端口。

### TLS 证书

服务器默认每次启动都会在内存中生成一份自签名证书（RSA 2048、有效期仅 2 小时、不落盘），配合客户端默认的证书校验行为无法直接使用——这也是上面警告"默认配置不安全"的原因。

也可以用 `-cert`/`-key` 加载一份真实证书：

```
./anytls-server -l 0.0.0.0:8443 -p 密码 -cert fullchain.pem -key privkey.pem
```

证书需要自行获取/续期并在续期后重启进程（`install.sh --domain ... --cloudflare-token ...` 可以自动完成这一整套流程，见下文"使用真实证书"）。

### Fallback（主动探测防护）

认证失败的连接（密码错误、非 anytls 流量等）默认会被直接断开。加上 `-fallback` 参数后，这类连接会被转发到本机指定端口的普通服务（例如一个正常运行的 nginx/静态网站），使得主动探测者看到的是一个正常的网页响应而不是连接中断：

```
./anytls-server -l 0.0.0.0:8443 -p 密码 -fallback 127.0.0.1:80
```

未配置 `-fallback` 时行为不变，认证失败仍直接断开连接。

fallback 目标只需要在本机监听普通 HTTP 即可——anytls-server 自己已经用真实证书完成了 TLS 握手，转发给 fallback 目标的是解密后的明文 HTTP 请求，浏览器/探测端看到的仍然是一次完整、合法的 HTTPS 响应。一个最简单的 nginx + 静态页示例见 [`examples/fallback`](./examples/fallback)。

用 `install.sh` 部署时这一套默认会自动配置好（见下文），不需要手动照着这个示例操作；只有不通过 `install.sh`、自己跑二进制/写 systemd 单元时才需要手动参考这个示例。

### 一键安装（systemd）

`install.sh` / `update.sh` 使用 GitHub Release 上由 CI 交叉编译好的二进制（见 [`.github/workflows/release.yml`](./.github/workflows/release.yml)，`amd64`/`arm64` Linux），下载后校验 `SHA256SUMS` 再安装，**不需要本机安装 Go、也不需要 clone 源码**，只需能访问 github.com：

```
curl -fsSL https://raw.githubusercontent.com/Jackchen0514/anytls-go/main/install.sh | sudo bash
```

或先下载脚本本身再执行：

```
sudo bash install.sh                                  # 随机生成密码与管理 API Key
sudo bash install.sh -p 自定义密码 --api-key 自定义Key   # 指定密码/Key
sudo bash install.sh --no-api                          # 不启用管理 API
sudo bash install.sh --version v0.0.14                 # 安装指定版本，默认 latest
sudo bash install.sh --no-fallback                     # 不自动配置本地 fallback 网站
sudo bash install.sh --fallback-port 8081              # fallback 站点改用其他本机端口（默认 8080）
```

脚本会下载对应架构的预编译包并安装 `anytls-server` / `anytls-client` 到 `/usr/local/bin`，创建独立系统用户运行服务，写入并启用 `anytls-server` systemd 服务，密码/API Key 会保存在 `/etc/anytls/credentials.env`（重复运行 `install.sh` 不会更换已生成的密码/Key）。若无法访问 GitHub 下载预编译包，脚本会直接报错退出，不会回退到本地编译；此时可自行 clone 仓库后用 `go build ./cmd/server` / `./cmd/client` 编译安装。

默认还会自动配置好上一节所说的 fallback（除非加 `--no-fallback`）：如果本机没有 nginx 会自动安装（`apt-get`/`dnf`/`yum`，识别不了包管理器则跳过并给出提示），部署一个占位页面并让 nginx 监听 `127.0.0.1:<--fallback-port>`（默认 `8080`，只监听本地回环，不会跟这台机器上已有的其他网站抢端口），再把 `-fallback` 接到 `anytls-server` 的 systemd 单元里。这一步失败（装不了 nginx、端口被占用等）只会打印警告，不影响 anytls-server 本身正常安装。重新运行 `install.sh` 会把 fallback 站点的配置和页面内容重置回脚本自带的默认版本，覆盖掉你手动做的修改；卸载时 `uninstall.sh` 会一并移除这个 nginx 站点配置（不会连 nginx 本身也卸载）。

#### 使用真实证书（可选）

默认情况下服务器每次启动都会生成一个自签名证书（见下方"TLS 证书"一节），客户端必须跳过证书校验才能连接。如果你有一个用 Cloudflare 解析的域名，可以让 `install.sh` 通过 Let's Encrypt DNS-01 挑战自动签发真实证书：

```
sudo bash install.sh --domain your.domain.com --cloudflare-token CF_API_TOKEN
```

- `--cloudflare-token` 需要一个具备该域名所在 Zone 的 `Zone:DNS:Edit` 权限的 Cloudflare API Token（在 Cloudflare 后台"My Profile → API Tokens"创建）。
- 首次运行会安装 [acme.sh](https://github.com/acmesh-official/acme.sh) 到 `/etc/anytls/acme.sh`（隔离安装，不影响系统其他部分），并设置好自动续期的 cron 任务；续期成功后会自动重启 `anytls-server` 服务加载新证书，无需人工干预。
- 证书文件安装在 `/etc/anytls/tls/`，仅 `anytls` 系统用户可读。
- 使用真实证书后，客户端应使用**默认设置**（不要加 `-insecure`）以获得真正的中间人防护；打印出的连接链接已经带上了正确的 `?sni=` 参数。
- 该流程需要能访问 GitHub（下载 acme.sh 源码）、Cloudflare API 与 Let's Encrypt，且要求 `--domain` 指定的域名已经在使用 Cloudflare 作为 DNS 服务商。
- 管理网页/API（`-api-listen`）会**自动复用同一份证书**改用 HTTPS 提供服务（服务端只要配置了 `-cert`/`-key` 就会这样，无需额外参数）；未配置真实证书时管理页仍是明文 HTTP。默认 `-api-listen` 只监听 `127.0.0.1`，若要从公网用域名直接访问管理页，还需要另外把它改成监听公网地址（例如手动改 systemd 单元里的 `-api-listen`），证书这块是自动的，是否对公网开放访问仍需你自己决定。

升级到最新版本：

```
sudo bash update.sh                  # 下载最新 release 并重启服务
sudo bash update.sh --version v0.0.14   # 安装/回滚到指定版本
```

卸载：

```
sudo bash uninstall.sh          # 卸载服务与二进制，保留用户数据库与凭据
sudo bash uninstall.sh --purge  # 同时清除用户数据库、凭据与系统用户
```

维护者发布新版本：推送形如 `v0.0.14` 的 tag 即可触发 CI 自动交叉编译并创建 GitHub Release（`git tag v0.0.14 && git push origin v0.0.14`）。

### 多用户与管理 API

示例服务器内置了基于 SQLite 的多用户支持：每个用户拥有独立密码，并可分别限制流量、IP 数与连接数。

```
./anytls-server -l 0.0.0.0:8443 -p 密码 -db anytls.db -api-listen 127.0.0.1:8080 -api-key API密钥
```

- `-p` 仅在用户数据库为空时用于引导一个不限量的 `default` 用户，之后应通过管理 API 创建/管理用户。
- `-db` 用户数据库文件路径（默认 `anytls.db`）。
- `-api-listen` 管理 API 监听地址，留空则不启动管理 API。
- `-api-key` 管理 API 所需的静态密钥（`Authorization: Bearer <key>`），启用 `-api-listen` 时必填。

启用 `-api-listen` 后，访问 `http://<api-listen>/`（如 `http://127.0.0.1:8080/`）即可打开内置的简易管理网页。首次访问会显示登录页，输入 API Key 校验通过后才能看到用户列表；登录状态保存在浏览器本地，可随时点击「退出登录」清除。登录后可创建/编辑/删除用户，查看实时流量与 IP/连接数用量，并手动重置流量；每个用户可设置到期日期（留空为永不过期），到期后自动拒绝连接；每个用户可点击「订阅」查看连接链接与二维码（客户端扫码即可导入），二维码在浏览器本地生成，无需联网。首次打开会用当前访问管理页的地址猜测服务器地址，若通过 SSH 隧道等方式访问、猜测的地址不对，可在弹窗里手动改成服务器真实可连接的地址（会记住，下次自动带出）。该页面外壳本身无需鉴权即可加载，登录态验证与之后的所有数据请求都由浏览器带着你输入的 Key 向 `-api-key` 校验，故仍应只在受信网络中开放此端口，或用反向代理加一层访问控制。

用户超出流量/IP 数/连接数限制、或账号已到期后，服务器会拒绝其新建的流/连接（已限流的用户的存量会话中，新的代理流也会被立即拒绝，账号到期后已打开的会话也会在下一次读写时被主动断开）。

管理 API（JSON）：

| 方法与路径 | 说明 |
|--|--|
| `GET /api/users` | 列出所有用户及其实时用量 |
| `POST /api/users` | 创建用户，字段：`username`、`password`（必填），`enabled`、`traffic_limit_bytes`、`ip_limit`、`conn_limit`、`traffic_reset_cycle`(`none`/`daily`/`monthly`)、`expires_at`(RFC3339 时间戳，如 `2026-12-31T23:59:59Z`，留空/不传 = 永不过期)（可选） |
| `GET /api/users/{id}` | 查询单个用户及用量 |
| `PUT /api/users/{id}` | 部分更新用户字段 |
| `DELETE /api/users/{id}` | 删除用户 |
| `POST /api/users/{id}/reset-traffic` | 手动重置该用户已用流量 |

### 示例客户端

```
./anytls-client -l 127.0.0.1:1080 -s 服务器ip:端口 -p 密码
```

`127.0.0.1:1080` 为本机 Socks5 代理监听地址，理论上支持 TCP 和 UDP(通过 udp over tcp 传输)。

v0.0.12 版本起，示例客户端可直接使用 URI 格式:

```
./anytls-client -l 127.0.0.1:1080 -s "anytls://password@host:port"
```

默认会校验服务器证书，若服务器用的是默认自签名证书（未配置 `-cert`/`-key`），需要加 `-insecure` 跳过校验：

```
./anytls-client -l 127.0.0.1:1080 -s 服务器ip:端口 -p 密码 -insecure
```

## 第三方兼容软件

### sing-box

https://github.com/SagerNet/sing-box

它包含了 anytls 协议的服务器和客户端。

### mihomo

https://github.com/MetaCubeX/mihomo

它包含了 anytls 协议的服务器和客户端。

### iOS 独立客户端实现

`Shadowrocket` 2.2.65+ 实现了 anytls 协议的客户端。

`Stash` `Loon` 等客户端已跟进适配。

这些客户端的具体实现细节与协议规范的契合度由各自维护团队负责，其细节处理路径与边界场景应对可能因版本而异，本项目未作验证。
