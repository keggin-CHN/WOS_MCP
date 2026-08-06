# WOS & CNKI MCP Server

This is an MCP (Model Context Protocol) Server for retrieving literature from Web of Science (WOS) and CNKI. It provides AI agents (like Claude Desktop or Cherry Studio) with direct access to academic databases.

## Features

- **Web of Science Search**: Search and extract detailed literature metadata.
- **CNKI Search**: Seamlessly retrieve Chinese literature.
- **Auto-Login via Shibboleth/SSO**: Configurable headless browser session for institution-based logins.
- **Flexible Deployment**: Run locally as a compiled Windows Executable (`.exe`) or deploy as an SSE server on a Linux machine.

## Quick Start (Local Windows EXE)

You can find the pre-compiled `WOS_MCP.exe` in the GitHub Actions artifacts, or compile it yourself.

1. Create a `config.json` file in the same directory as the `.exe`:
```json
{
    "username": "YOUR_INSTITUTION_USERNAME",
    "password": "YOUR_INSTITUTION_PASSWORD",
    "port": 7861,
    "listen_public": false
}
```
2. Double-click `WOS_MCP.exe`. A terminal window will briefly appear and then hide itself after 20 seconds, running silently in the background.

## Build from Source

You can build the Windows executable yourself using PyInstaller:

```bash
pip install -r requirements.txt
pyinstaller WOS_MCP.spec
```
The compiled executable will be located in the `dist/` folder.
*Note: We have also configured GitHub Actions to automatically compile the `.exe` upon every push.*

## Server Deployment (Linux / SSE Mode)

If you want to host this MCP server on a remote Linux instance:

1. Clone the repository to your server.
2. Edit `config.json` with your credentials and set `"listen_public": true`.
3. Use the provided deployment script to install dependencies and run the server via `uvicorn`:
```bash
dos2unix deploy_remote.sh
./deploy_remote.sh
```

## Acknowledgments

Special thanks to the following open-source repositories that inspired or provided foundational ideas for this project:
- [cnki-mcp-server](https://github.com/xxxxchaos/cnki-mcp-server)
- [wos-skills](https://github.com/cookjohn/wos-skills)