# Web of Science MCP Server

这是一个用于与 Web of Science 交互的 MCP (Model Context Protocol) 服务端。
由于它基于 Cookie 抓包模拟网页请求（而不是使用官方 API Key），因此需要进行一些手动配置。

## 1. 环境准备

首先，进入项目目录并安装依赖：
```bash
pip install -r requirements.txt
```

## 2. 鉴权配置 (自动提取)

我们为您提供了一个自动提取脚本。您无需再手动复制 Cookie 和 SID！

运行以下命令，脚本会自动扫描您抓包的文件夹（`C:\code\wos\1` 和 `C:\code\wos\2`），提取出最新的 Cookie 和 SID，并保存到根目录下的 `.env` 文件中：

```bash
python extract_auth.py
```

*如果提取成功，您会看到提示，并且生成了一个 `.env` 文件。MCP Server 启动时会自动读取这个文件里的鉴权信息。*

---

**（可选）手动配置：**
如果自动提取失败，您可以手动配置环境变量：
- `WOS_SID`: 抓包 URL 中的 `SID=` 后面的值。
- `WOS_COOKIE`: 抓包文件中的所有 `cookie` 字段拼接成的字符串（以分号 `; ` 隔开）。

在 Windows PowerShell 中：
```powershell
$env:WOS_COOKIE="您的Cookie"
$env:WOS_SID="您的SID"
```

## 3. 运行与集成

您可以直接在本地运行测试：
```bash
python server.py
```
*(注意：直接运行后，程序会挂起并等待标准输入，这是 MCP Server 的标准行为)*

**与 Claude Desktop 集成：**
在您的 Claude Desktop 配置文件 (`claude_desktop_config.json`) 中添加以下内容：
```json
{
  "mcpServers": {
    "webofscience": {
      "command": "python",
      "args": ["C:\\code\\wos\\wos_mcp\\server.py"]
    }
  }
}
```
*因为我们有了 `.env` 文件，所以 `claude_desktop_config.json` 里不需要再手动配环境变量了！*
