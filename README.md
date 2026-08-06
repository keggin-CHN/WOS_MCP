# WOS & CNKI MCP Server

这是一个强大的 MCP (Model Context Protocol) 服务端，支持直接检索 **Web of Science (WOS)** 以及 **CNKI (中国知网)** 的学术文献。
它可以无缝集成到各种支持 MCP 的 AI Agent（如 Claude Desktop 或 Cherry Studio）中，让您的 AI 助手具备直接搜索、阅读和分析顶尖学术期刊的能力。

## 主要功能

- **Web of Science 检索**: 支持核心合集文献搜索、元数据提取。
- **CNKI 检索**: 支持知网中文文献的高效检索。
- **自动登录/SSO 支持**: 包含应对机构登录（如 Shibboleth/SSO）和验证码处理的逻辑。
- **多端支持**: 既可以在本地作为免安装的 Windows `exe` 运行，也可以作为独立服务端部署在 Linux 服务器上。

---

## 快速使用 (Windows 本地 EXE)

如果您是 Windows 用户，无需安装 Python 环境，可以直接使用打包好的执行文件。
（每次代码更新，GitHub Actions 会自动编译最新的 `WOS_MCP.exe`，您可以在 Actions 的 Artifacts 中下载）

1. 在 `WOS_MCP.exe` 同级目录下，新建一个 `config.json` 文件：
```json
{
    "username": "您的账号",
    "password": "您的密码",
    "port": 7861,
    "listen_public": false
}
```
2. 双击运行 `WOS_MCP.exe`。
*(程序启动后会有大约 20 秒的黑窗口，随后将自动隐藏并在后台静默运行提供服务。)*

---

## 服务器部署 (Linux Shell 一键脚本)

如果您想将该服务部署到您的远端服务器（如 Ubuntu/CentOS），可以直接使用本仓库提供的 `deploy_remote.sh` 一键脚本：

1. 克隆本仓库到服务器。
2. 编辑 `config.json` 填入您的配置，并将 `"listen_public"` 设置为 `true`，以允许外网访问（或者根据需要保持 `false` 并通过代理访问）。
3. 赋予脚本执行权限并一键启动：
```bash
dos2unix deploy_remote.sh
chmod +x deploy_remote.sh
./deploy_remote.sh
```
该脚本会自动为您创建虚拟环境、安装所有依赖，并在后台以守护进程模式启动服务。

---

## 从源码构建 (开发者)

如果您希望自行编译 EXE 文件：

```bash
pip install -r requirements.txt
pyinstaller WOS_MCP.spec
```
编译好的程序将在 `dist/` 目录下生成。

---

## 致谢 (Acknowledgments)

本项目在开发过程中，深受开源社区的启发与帮助，特此致谢以下优秀的开源项目：

- [xxxxchaos/cnki-mcp-server](https://github.com/xxxxchaos/cnki-mcp-server)
- [cookjohn/wos-skills](https://github.com/cookjohn/wos-skills)