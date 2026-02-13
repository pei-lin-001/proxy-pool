# Proxy Pool

[English](README.md) | 简体中文

Proxy Pool 是一个自建的代理节点池管理工具，基于 [sing-box](https://github.com/SagerNet/sing-box)。
它可以把你的节点/订阅聚合成一个或多个**HTTP 代理入口**，并提供健康检查、自动故障转移、WebUI 管理等能力。

> 说明：本仓库最初基于上游项目 `easy_proxies`，后续会持续演进。

## 主要特性

- **Pool / 多端口 / 混合模式**
- **订阅链接**（Base64 / Clash YAML / 纯文本）+ 手动刷新 + 可选定时刷新
- **WebUI**：节点状态、延迟探测、一键导出、系统设置、节点增删改查、重载配置
- **GeoIP 地区筛选**：自动解析节点服务器 IP 的地区信息，并生成可选的地区列表（单选/多选）
- **自动健康检查**：启动探测 + 定时探测；自动拉黑/解除拉黑
- **端口保留**：重载后已有节点尽量保持原端口不变

## 快速开始（本地）

1) 准备配置文件：

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
```

2) 修改 `config.yaml`（最小示例）：

```yaml
mode: pool
listener:
  address: 0.0.0.0
  port: 2323
  username: username
  password: password
management:
  enabled: true
  listen: 0.0.0.0:9090
  probe_target: www.apple.com:80

# 三选一或混合：
# nodes_file: nodes.txt
subscriptions:
  - "https://example.com/subscribe"
```

3) 编译运行：

```bash
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies
./easy-proxies --config config.yaml
```

4) 使用方式：

- 代理入口（Pool）：`http://username:password@127.0.0.1:2323`
- WebUI：`http://127.0.0.1:9090`

如果你**暂时没有配置任何节点**，WebUI 也会正常启动。你可以在 WebUI 里添加订阅/节点，然后点击 **重载配置** 启动内核。

## 快速开始（Docker）

本仓库的 `docker-compose.yml` 会从源码构建镜像（所以你修改代码后会真正生效）。

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
./start.sh
```

更新方式：

```bash
git pull --ff-only
./start.sh
```

## WebUI 功能

打开 `http://<management.listen>`：

- 首页：节点状态、延迟、活跃连接、拉黑状态
- 导出：导出可用的代理入口链接
- 节点管理：添加/编辑/删除节点，然后重载生效
- 系统设置：
  - external IP（导出链接时用于替换 `0.0.0.0`）
  - 探测目标
  - 订阅链接与刷新配置
  - 节点过滤（按名称 / 按 GeoIP / 名称或 GeoIP）

## 地区筛选（GeoIP）

### WebUI（推荐）

1) 打开 WebUI → 点击右上角 **⚙️ 系统设置**  
2) 找到 **地区快捷选择** → 点击 **刷新列表**（自动解析 GeoIP）  
3) 勾选一个或多个地区（勾选“单选”可限制一次只能选一个）  
4) 保存设置 → 如提示需要重载，点击 **重载配置** 使筛选生效

勾选地区后会自动：

- 将 `node_filter.target` 设置为 `geo`
- 将所选地区同步到 `node_filter.include`

### 配置文件

```yaml
node_filter:
  target: geo         # name | geo | name_or_geo
  include: ["US", "美国", "香港", "洛杉矶"]
  exclude: []
  use_regex: false
```

注意：

- GeoIP 信息为**尽力而为**，不会写入/保存到 `config.yaml`。
- GeoIP 默认使用 `ipwho.is` 查询；如果你的网络无法访问该 API，地区信息可能显示为空/未知。

## 订阅与刷新

- 在 **系统设置 → 订阅链接** 中添加订阅
- 点击 **刷新订阅**（或在 `subscription_refresh` 启用定时刷新）

重要提示：刷新订阅/重载配置会重启 sing-box 内核，**会中断现有连接**。

## 端口说明

- `2323`：Pool 入口（pool / hybrid）
- `24000+`：多端口入口（multi-port / hybrid）
- `9090`：WebUI（可通过 `management.listen` 修改）

## 常见问题

- 订阅拉取失败/全部异常：请检查系统代理或环境变量（`http_proxy`/`https_proxy`/`all_proxy`），代理循环会导致订阅/GeoIP 请求失败。
- GeoIP 解析为空：可能节点域名无法解析到公网 IP，或 GeoIP API 不可达；可稍后重试或改用“按名称”筛选。
- WebUI 远程访问不到：将 `management.listen` 绑定到 `0.0.0.0`，并确认防火墙放行对应端口。
- 保存设置失败（permission denied）：如果 `config.yaml` / `nodes.txt` 是宿主机挂载进容器的文件，请确保它们对容器运行用户可写（或让容器以 root 运行）。

## 许可证

MIT License
