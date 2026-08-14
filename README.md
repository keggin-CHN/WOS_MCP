# WOS & CNKI MCP Server

用 **Go** 实现的 MCP 服务端，检索 **Web of Science** 与 **CNKI (中国知网)** 文献，并免人机验证获取 CNKI 文章摘要。

## 功能

- **WOS 检索**：核心合集文献搜索、元数据提取、Unpaywall Open Access 下载。
- **CNKI 检索 + 免验证摘要**：机构 SSO 纯协议登录拿到 `LID` 令牌，文章详情页直接放行、全程无人机验证；内置限流与"被拦自动重登"，抗知网频率风控。
- **双传输**：SSE (HTTP) + stdio，可接入 Claude Desktop 等 MCP 客户端。

## 快速开始 (Windows)

```bash
cd wos_mcp_go
go build -o wos_mcp_go.exe main.go cnki.go config.go extra_tools.go login.go slider_solver.go wos.go
./wos_mcp_go.exe
```

在 `wos_mcp_go.exe` 同级目录新建 `config.json`（参考 `config.example.json`）：

```json
{
    "username": "您的机构账号",
    "password": "您的密码",
    "port": 5000,
    "download_path": "download"
}
```

启动后 SSE 端点为 `http://127.0.0.1:5000/sse`（端口由 `port` 决定），stdio 同时开启。

## 接入 MCP 客户端（Claude Desktop）

**Windows**：`%APPDATA%\Claude\claude_desktop_config.json`，添加：

```json
{
  "mcpServers": {
    "academic-search": {
      "command": "C:\\完整路径\\wos_mcp_go.exe",
      "args": []
    }
  }
}
```

重启 Claude Desktop 后即可直接让它调用知网/WOS 搜索与摘要。

## 配置说明

| 字段 | 说明 |
|---|---|
| `username` / `password` | 机构 CAS 账号。也可用环境变量 `WOS_USERNAME` / `WOS_PASSWORD` 注入，不落盘 |
| `port` | SSE 监听端口，默认 5000 |
| `listen_public` | `true` 时监听 `0.0.0.0` 允许外网，默认 `127.0.0.1` |
| `download_path` | 下载目录 |
| `wos_sid` / `wos_cookies` / `cnki_cookies` | 会话，登录后自动刷新，无需手工填 |

> ⚠️ **密码安全**：登录后账号密码会以**明文**写入 `config.json`。请勿提交 `config.json`、`config.key` 到版本库；或改用环境变量注入。

## 致谢

- [xxxxchaos/cnki-mcp-server](https://github.com/xxxxchaos/cnki-mcp-server)
- [cookjohn/wos-skills](https://github.com/cookjohn/wos-skills)
