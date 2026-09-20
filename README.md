# WOS & CNKI MCP Server (Pure Go Version)

基于 **Go 语言**的学术文献 MCP (Model Context Protocol) 服务端，支持 **Web of Science (WoS)** 与 **CNKI (中国知网)** 文献检索、摘要获取、**PDF 下载后立即阅读**、**指定本地路径 PDF 读取**与**按页、按字符连续阅读全文**。PDF 解析使用纯 Go 库，无需 Python 或外部 PDF 工具。

---

## 🌟 核心特性

- 🚀 **100% 纯 Go 实现**：无需 Python 运行时或复杂第三方环境，单二进制文件开箱即用，资源占用低、启动极速。
- 🔍 **CNKI (中国知网) 全套工具链**：
  - **机构 SSO 纯协议认证**：自动完成 CAS + Shibboleth 统一认证并提取 `LID` 与全域凭证，无需手动抓包 Cookie。
  - **免人机验证获取摘要**：智能识别风控，文章详情页直接放行，内置请求限流与 Session 保活机制。
  - **知网文献 PDF 全自动下载**：逆向重定向与全域 Cookie 保活，支持一键按文章标题、关键词或 URL 下载完整 PDF。
- 📁 **安全沙盒与文献管理**：
  - **沙盒隔离机制**：自动将文献归档在 `download/` 目录下，严格防范路径穿越。
  - **多级分类目录**：支持按时间或主题自定义子文件夹归档，如 `topic/subtopic`。
  - **分页全文阅读**：使用 PDF 对象解析和 Unicode 字体映射，返回物理页码、文本覆盖范围、未提取页面和续读参数。
  - **指定路径读取**：支持 Windows / Linux / macOS 上的 PDF 绝对路径、`file://` URI，以及下载目录内的相对路径。
  - **连续阅读**：默认每次返回 20000 个 Unicode 字符，超出后提供 `next_call`；复用已解析文本，避免每次续读重新解析文档。
- 📖 **面向 AI 的全文工作流**：MCP 初始化指引、工具描述、检索结果和下载结果均明确区分摘要与全文，引导 AI 筛选论文后主动下载、连续阅读，再完成研究或综述。
- 🌐 **Web of Science (WoS) 检索**：
  - 核心合集文献搜索、结构化元数据提取、国际标准引文生成与 Unpaywall Open Access 支持。
- 🔌 **双传输协议支持**：
  - 支持 **SSE (HTTP)** 与 **stdio** 双模式，完美兼容 Claude Desktop、Cursor、Antigravity IDE、Cherry Studio 等各类 MCP 客户端。

---

## 🛠️ MCP 工具清单

| 工具名称 | 功能描述 | 主要参数 |
|---|---|---|
| `download_cnki_paper` | 下载知网 PDF，默认立即返回正文分段和续读参数 | `query`, `title`, `url`, `subfolder`, `extract_text`, `max_chars` |
| `list_downloaded_papers` | 查看 `download` 沙盒目录下的已下载文献与文件清单 | `subfolder` (可选) |
| `read_paper_content` | 读取本地 PDF，或沙盒内 TXT / JSON / Markdown，支持连续分段 | `file_path`, `start_page`, `end_page`, `offset`, `max_chars` |
| `read_pdf` | 直接读取指定路径 PDF，无需先下载或搬移文件 | `file_path`, `start_page`, `end_page`, `offset`, `max_chars` |
| `search_cnki` | 检索中国知网中文文献，返回题录/摘要及全文下载建议 | `query`, `search_type`, `limit` |
| `get_cnki_paper_detail` | 获取知网论文详情（摘要、作者、关键词等） | `url`, `title` |
| `search_literature` | 检索 Web of Science 国际核心数据库文献 | `query`, `limit` |
| `get_wos_paper_details` | 获取指定 WoS 文献的完整元数据 | `wos_id` |
| `format_citation` | 生成规范的学术引用字符串 | `title`, `authors`, `source`, `year` |
| `download_literature` | 统一下载并开始阅读，支持 DOI / WoS ID / 知网标题或 URL / PDF 直链 | `doi_or_wosid`, `doi`, `title`, `query`, `url`, `subfolder`, `extract_text`, `max_chars` |

### 全文阅读示例

读取指定位置的 PDF（`read_pdf` 或 `read_paper_content`）：

```json
{"file_path": "D:/文献/论文.pdf"}
```

同样支持 `file:///D:/文献/论文.pdf`、`/home/user/papers/paper.pdf`、`~/papers/paper.pdf`。**路径指 MCP 服务器所在机器**；远程 SSE 服务不能直接读取客户端电脑的硬盘。相对路径（如 `topic/paper.pdf`）始终以 `download_path` 为根目录。

只读某个页码范围，或控制每次返回的字符数：

```json
{"file_path": "D:/文献/论文.pdf", "start_page": 3, "end_page": 8, "max_chars": 12000}
```

`start_page` / `end_page` 是从 1 开始的 PDF 物理页码，结束页包含在内。省略它们时选择整篇文献。下载工具默认 `extract_text=true`，同样自动返回首段正文；只有单纯存档时才设为 `false`。

返回值包含：

| 字段 | 含义 |
|---|---|
| `pages` | 本次返回的正文，包含 `page_number`、页内字符范围和 `text` |
| `total_chars` / `returned_chars` | 所选页范围的总字符数 / 本次返回字符数 |
| `offset` / `next_offset` | 在固定页范围内的 Unicode 字符偏移，从 0 开始 |
| `has_more` / `next_call` | 是否还有正文，以及可直接执行的下一次工具调用 |
| `range_covers_document` | 所选页范围是否覆盖整个文档 |
| `full_text_included` | **本次响应**是否包含所有页面的全部已提取文本，不能把最后一段误认为全文 |
| `extraction_complete` / `pages_without_text` | 是否所有页面均提取出文本，以及未提取出文本的页码 |

**AI 的推荐流程**：检索 → 筛选相关论文 → 下载并读取 → 按 `next_call` 逐段续读到 `has_more=false` → 带页码给出分析。仅要求题录、引用或摘要时无需下载所有结果。MCP 会提供明确指引和后续调用，但具体是否执行仍由 AI 客户端决定。

PDF 阅读只提取文本层，暂不提供 OCR 或图像阅读。扫描页、纯图页、空白页会明确列出；全部没有文本时工具返回错误及诊断。图表、公式和多栏版式需要额外核对，不能将文本提取等同于完整视觉阅读。单个文件上限 100 MiB，PDF 上限 2000 页、解码文本上限 32 MiB，单次最多返回 100000 字符。CAJ 需要先转换为 PDF。

下载会校验 HTTP 状态、PDF 文件头和结束标记；失败时清理临时文件，不会把登录网页保存为“成功下载的 PDF”。同名文献使用独立文件名。开放获取会尝试多个 PDF 候选链接。

---

## 🚀 快速开始

### 1. 编译构建

使用 `wos_mcp_go/go.mod` 声明的 Go 版本（当前为 Go 1.26.5）：

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
>
> DOI / WoS 的开放获取下载还需在配置中填写 `unpaywall_email`，或设置环境变量 `UNPAYWALL_EMAIL` 为自己的有效联系邮箱。知网下载、PDF 直链下载和本地 PDF 阅读不需要此项；本地阅读也不需要机构账号。

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
| `local_pdf_roots` | string[] | 可读取的沙盒外 PDF 目录列表。默认空列表且 `listen_public=false` 时允许读取服务机器上的任意 PDF；配置列表后仅允许这些目录。`listen_public=true` 且列表为空时仅允许下载沙盒。相对目录以配置文件位置为基准。 |
| `unpaywall_email` | string | Unpaywall 联系邮箱；环境变量 `UNPAYWALL_EMAIL` 优先，仅 DOI/WoS 开放获取下载使用 |
| `wos_sid` / `cnki_cookies` | object | 运行过程中自动维护与刷新的会话 Cookie 缓存，无需手动填写 |

例如将外部阅读限制在两个文献目录：

```json
{"local_pdf_roots": ["D:/文献", "C:/Users/yourname/Documents/Papers"]}
```

沙盒外只开放 PDF 的只读访问，下载仍写入沙盒。会话刷新会保留上述配置。更新二进制后，请重启 MCP 并在客户端重新连接，刷新工具列表与初始化指引。

### 开发验证

```bash
cd wos_mcp_go
go test ./...
go vet ./...
go build -o wos_mcp_go.exe .
```

自动化测试覆盖中文字体映射、多页分段续读、指定路径、阅读覆盖状态、损坏 PDF、路径边界、下载响应校验、开放获取备用链接，以及 MCP 初始化/工具发现/调用。测试使用本地 PDF 和模拟 HTTP，不依赖机构账号或真实知网/WoS 网络。

---

## 📄 开源许可

本项目遵循 MIT 开源许可证。
