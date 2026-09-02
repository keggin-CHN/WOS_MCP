# WOS & CNKI MCP Server (Pure Go Version)

基于 **Go 语言纯原生实现**的高性能学术文献 MCP (Model Context Protocol) 服务端，支持 **Web of Science (WoS)** 与 **CNKI (中国知网)** 文献检索、免人机验证摘要获取、**知网文献 PDF 自动下载**、**沙盒文件管理**与**纯 Go PDF 文本流提取**。

---

## 🌟 核心特性

- 🚀 **100% 纯 Go 实现**：无需 Python 运行时或复杂第三方环境，单二进制文件开箱即用，资源占用低、启动极速。
- 🔍 **CNKI (中国知网) 全套工具链**：
  - **机构 SSO 纯协议认证**：自动完成 CAS + Shibboleth 统一认证并提取 `LID` 与全域凭证，无需手动抓包 Cookie。
  - **免人机验证获取摘要**：智能识别风控，文章详情页直接放行，内置请求限流与 Session 保活机制。
  - **知网文献 PDF 全自动下载**：逆向重定向与全域 Cookie 保活，支持一键按文章标题、关键词或 URL 下载完整 PDF。
- 📁 **安全沙盒与文献管理**：
  - **沙盒隔离机制**：自动将文献归档在 `download/` 目录下，严格防范路径穿越。
  - **多级分类目录**：支持按时间或主题自定义子文件夹归档。
  - **纯 Go PDF 文本流提取**：无需外部工具即可提取 PDF 前数千字正文内容，便于 AI 直接研读。
- 🌐 **Web of Science (WoS) 检索**：
  - 核心合集文献搜索、结构化元数据提取、国际标准引文生成与 Unpaywall Open Access 支持。
- 🔌 **双传输协议支持**：
  - 支持 **SSE (HTTP)** 与 **stdio** 双模式，完美兼容 Claude Desktop、Cursor、Antigravity IDE、Cherry Studio 等各类 MCP 客户端。

---

## 🛠️ MCP 工具清单

| 工具名称 | 功能描述 | 主要参数 |
|---|---|---|
| `download_cnki_paper` | 搜索并下载知网文献 PDF 全文至本地沙盒，可选提取正文文本 | `query`, `title`, `url`, `subfolder`, `extract_text` |
| `list_downloaded_papers` | 查看 `download` 沙盒目录下的已下载文献与文件清单 | `subfolder` (可选) |
| `read_paper_content` | 读取沙盒目录下指定文献的文本内容（支持 PDF / TXT / JSON） | `file_path`, `max_chars` |
| `search_cnki` | 检索中国知网中文文献 | `query`, `limit` |
| `get_cnki_paper_detail` | 获取知网论文详情（摘要、作者、关键词等） | `url`, `title` |
| `search_literature` | 检索 Web of Science 国际核心数据库文献 | `query`, `limit` |
| `get_wos_paper_details` | 获取指定 WoS 文献的完整元数据 | `wos_id` |
| `format_citation` | 生成规范的学术引用字符串 | `title`, `authors`, `source`, `year` |
| `download_literature` | 统一文献下载接口（自动路由 CNKI 或 WoS） | `doi_or_wosid`, `title`, `url`, `subfolder` |

---

## 🚀 快速开始

### 1. 编译构建

确保已安装 Go 1.22+：

```bash
cd wos_mcp_go
go build -o wos_mcp_go.exe .
```

### 2. 配置文件

在 `wos_mcp_go.exe` 同级目录下创建 `config.json`（可参考 `config.example.json`）：

```json
{
    "username": "您的机构账号",
    "password": "您的密码",
    "port": 5000,
    "download_path": "download"
}
```

> 💡 **提示**：也可直接使用环境变量注入账号，无需将密码保存在配置文件中：
> - `WOS_USERNAME=your_username`
> - `WOS_PASSWORD=your_password`

### 3. 运行服务

```bash
# Windows
.\wos_mcp_go.exe

# Linux / macOS
./wos_mcp_go
```

启动后：
- **SSE 端点**：`http://127.0.0.1:5000/sse`
- **stdio 模式**：同时在标准输入输出监听 MCP 协议请求。

---

## 💻 接入 MCP 客户端

### 1. Claude Desktop

编辑配置文件 `%APPDATA%\Claude\claude_desktop_config.json` (Windows) 或 `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS)：

```json
{
  "mcpServers": {
    "academic-search": {
      "command": "C:\\path\\to\\wos_mcp_go\\wos_mcp_go.exe",
      "args": []
    }
  }
}
```

### 2. Cursor / Antigravity IDE / 其它 SSE 客户端

直接添加 SSE 类型 MCP Server：
- **URL**: `http://127.0.0.1:5000/sse`

---

## 📋 配置参数说明

| 字段 | 类型 | 说明 |
|---|---|---|
| `username` | string | 机构 CAS 统一认证账号 |
| `password` | string | 机构 CAS 密码 |
| `port` | number | SSE 服务监听端口（默认 `5000`） |
| `listen_public` | boolean | 为 `true` 时监听 `0.0.0.0`，默认 `false` (监听 `127.0.0.1`) |
| `download_path` | string | 文献下载沙盒根目录（默认 `download`） |
| `wos_sid` / `cnki_cookies` | object | 运行过程中自动维护与刷新的会话 Cookie 缓存，无需手动填写 |

---

## 📄 开源许可

本项目遵循 MIT 开源许可证。
