# WOS & CNKI MCP Server

这是一个强大的 MCP (Model Context Protocol) 服务端，支持直接检索 **Web of Science (WOS)** 以及 **CNKI (中国知网)** 的学术文献。

## 主要功能

- **Web of Science 检索**: 支持核心合集文献搜索、元数据提取。
- **CNKI 检索**: 支持知网中文文献的高效检索。
- **自动登录/SSO 支持**: 包含应对南京林业大学登录，且底层实现了**自动极速突破知网高频弹出的动态滑块验证码**机制，最高支持 100 级极速高并发，真正零干预！

---

## 快速使用 (Windows 本地 EXE)

见 Releases 页面下载最新版。

1. 在 `wos_mcp.exe` 同级目录下，新建一个 `config.json` 文件：
```json
{
    "username": "您的账号",
    "port": 7861,
    "listen_public": false
}
```
2. 双击运行 `wos_mcp.exe`。

> ⚠️ **密码安全说明**：
> - 密码**不要**明文写在 `config.json` 里。首次登录后程序会把密码加密写入 `password_enc` 字段（AES-256-GCM），密钥保存在同目录 `config.key`（权限 600）或环境变量 `WOS_CONFIG_KEY` 中。
> - 也可以完全不写密码：通过环境变量注入 `WOS_USERNAME` / `WOS_PASSWORD`。
> - 旧版明文 `password` 字段仍会被兼容读取，但保存时会自动迁移为密文。

---

## 服务器部署 (Linux Shell 一键脚本)

如果您想将该服务部署到您的远端服务器（如 Ubuntu/CentOS），可以直接使用本仓库提供的 `deploy_remote.sh` 一键脚本：

1. 克隆本仓库到服务器。
2. 编辑 `config.json` 填入您的配置，并将 `"listen_public"` 设置为 `true`，以允许外网访问。
3. 确保服务器已安装 Go 1.22 及以上版本环境。
4. 赋予脚本执行权限并一键启动：
```bash
dos2unix deploy_remote.sh
chmod +x deploy_remote.sh
./deploy_remote.sh
```
该脚本会自动为您编译 Go 源码，并在后台以守护进程模式启动二进制服务端！

---

## 从源码构建

如果您希望自行编译 EXE 文件：

```bash
cd wos_mcp_go
go mod tidy
go build -o ../dist/wos_mcp.exe
```
编译好的程序将在 `dist/` 目录下生成。

---

## 致谢 (Acknowledgments)

本项目在开发过程中，深受开源社区的启发与帮助，特此致谢以下优秀的开源项目：

- [xxxxchaos/cnki-mcp-server](https://github.com/xxxxchaos/cnki-mcp-server)
- [cookjohn/wos-skills](https://github.com/cookjohn/wos-skills)