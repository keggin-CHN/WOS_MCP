package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var (
	querySafeRe = regexp.MustCompile(`[^A-Za-z0-9 \-\*\?\.\'"\x{4e00}-\x{9fff}]`)
	yearRangeRe = regexp.MustCompile(`^\d{4}$|^\d{4}-\d{4}$`)
	docTypeRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z \-]*$`)
	wosIDRe     = regexp.MustCompile(`^WOS:[A-Z0-9]+$`)
	cnkiClient  = NewCnkiClient()
)

func sanitizeQuery(query string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("query cannot be empty")
	}
	cleaned := querySafeRe.ReplaceAllString(query, " ")
	re := regexp.MustCompile(`\s+`)
	cleaned = strings.TrimSpace(re.ReplaceAllString(cleaned, " "))
	if cleaned == "" {
		return "", fmt.Errorf("query is empty after sanitization")
	}
	return cleaned, nil
}

func backgroundMaintainer() {
	log.Println("[Background] Session maintainer started.")
	for {
		for {
			_, _, err := ensureWosSession()
			if err == nil {
				log.Println("[Background] WOS Session ensured successfully.")
				break
			}
			log.Printf("[Background] WOS Session maintain failed: %v. Retrying in 30s...\n", err)
			time.Sleep(30 * time.Second)
		}

		for {
			err := cnkiClient.ensureSession()
			if err == nil {
				log.Println("[Background] CNKI Session ensured successfully.")
				break
			}
			log.Printf("[Background] CNKI Session maintain failed: %v. Retrying in 30s...\n", err)
			time.Sleep(30 * time.Second)
		}

		time.Sleep(2 * time.Hour)
	}
}

func setupServer() *server.MCPServer {
	s := server.NewMCPServer(
		"Academic_WoS_CNKI",
		"1.0.0",
		server.WithToolCapabilities(true),
	)

	s.AddTool(mcp.NewTool("search_literature",
		mcp.WithDescription("Search for literature on Web of Science."),
		mcp.WithString("query", mcp.Required(), mcp.Description("The search query string")),
		mcp.WithNumber("limit", mcp.Description("Number of results to return (default 10)")),
	), searchLiteratureHandler)

	s.AddTool(mcp.NewTool("get_wos_paper_details",
		mcp.WithDescription("Fetch full detailed metadata for a specific paper"),
		mcp.WithString("wos_id", mcp.Required(), mcp.Description("The Web of Science ID (e.g., WOS:000295471900004)")),
	), getWosPaperDetailsHandler)

	s.AddTool(mcp.NewTool("search_cnki",
		mcp.WithDescription("Search for Chinese literature on CNKI."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
		mcp.WithNumber("limit", mcp.Description("Number of results to return")),
	), searchCnkiHandler)

	s.AddTool(mcp.NewTool("download_literature",
		mcp.WithDescription("Download literature via Unpaywall OA links."),
		mcp.WithString("doi_or_wosid", mcp.Required(), mcp.Description("DOI or WOS ID")),
	), downloadLiteratureHandler)

	s.AddTool(mcp.NewTool("export_wos_papers",
		mcp.WithDescription("Export WOS papers."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
		mcp.WithNumber("limit", mcp.Description("Limit")),
		mcp.WithString("year_range", mcp.Description("Year Range")),
		mcp.WithString("doc_type", mcp.Description("Document Type")),
		mcp.WithString("format", mcp.Description("Format (default bibtex)")),
	), exportWosPapersHandler)

	s.AddTool(mcp.NewTool("get_cnki_paper_detail",
		mcp.WithDescription("获取知网论文详情（摘要、作者、关键词等），自动绕过验证码限制"),
		mcp.WithString("url", mcp.Required(), mcp.Description("CNKI paper URL")),
		mcp.WithString("title", mcp.Description("（可选）文章标题，提供时直接通过搜索获取详情，更可靠")),
	), getCnkiPaperDetailHandler)

	s.AddTool(mcp.NewTool("find_best_match",
		mcp.WithDescription("在知网搜索并返回最匹配结果"),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
		mcp.WithString("search_type", mcp.Description("搜索类型（主题/篇名/作者/关键词）")),
		mcp.WithNumber("limit", mcp.Description("结果数量 (default 5)")),
	), searchCnkiHandler)

	s.AddTool(mcp.NewTool("download_cnki_paper",
		mcp.WithDescription("下载中国知网 (CNKI) 文献 PDF 全文至本地 download 沙盒目录，并可提取正文前 3000 字文本"),
		mcp.WithString("url", mcp.Description("知网文献详情页 URL (如 https://kns.cnki.net/kcms2/article/abstract?v=...)")),
		mcp.WithString("title", mcp.Description("文章标题（若未提供 URL，将自动按标题精确搜索并下载）")),
		mcp.WithString("query", mcp.Description("检索关键词（若未提供 URL/标题，将自动搜索第 1 篇匹配文献并下载）")),
		mcp.WithString("subfolder", mcp.Description("存放子文件夹名称（可选，如不填则存入当天日期目录或默认目录）")),
		mcp.WithBoolean("extract_text", mcp.Description("是否同时提取并返回 PDF 前 3000 字纯文本供直接研读（默认 true）")),
	), downloadCnkiPaperHandler)

	s.AddTool(mcp.NewTool("list_downloaded_papers",
		mcp.WithDescription("查看 download 沙盒目录下的已下载文献与文件列表"),
		mcp.WithString("subfolder", mcp.Description("子文件夹路径（可选，留空查看整个 download 目录）")),
	), listDownloadedPapersHandler)

	s.AddTool(mcp.NewTool("read_paper_content",
		mcp.WithDescription("读取 download 沙盒目录下指定文献文件的文本内容（支持 PDF 纯文本流提取及 txt/json 读取）"),
		mcp.WithString("file_path", mcp.Required(), mcp.Description("文件在 download 目录下的相对路径 (如 2026-09-02/paper.pdf)")),
		mcp.WithNumber("max_chars", mcp.Description("最大读取字符数（默认 20000）")),
	), readPaperContentHandler)

	s.AddTool(mcp.NewTool("format_citation",
		mcp.WithDescription("Format citation string"),
		mcp.WithString("title", mcp.Required(), mcp.Description("Title")),
		mcp.WithString("authors", mcp.Required(), mcp.Description("Authors")),
		mcp.WithString("source", mcp.Required(), mcp.Description("Source")),
		mcp.WithString("year", mcp.Required(), mcp.Description("Year")),
	), formatCitationHandler)

	s.AddTool(mcp.NewTool("export_cnki_papers",
		mcp.WithDescription("Export CNKI papers"),
		mcp.WithString("papers_json", mcp.Required(), mcp.Description("Papers JSON string")),
		mcp.WithString("format", mcp.Description("Format")),
	), exportCnkiPapersHandler)

	s.AddResource(mcp.NewResource("cnki://status",
		"CNKI Server Status",
		mcp.WithMIMEType("application/json"),
	), getStatusResourceHandler)

	s.AddResource(mcp.NewResource("cnki://search-types",
		"CNKI Search Types",
		mcp.WithMIMEType("application/json"),
	), getSearchTypesResourceHandler)

	return s
}

func main() {
	// Read config for SSE port
	cfg := LoadConfig()
	portF, _ := cfg["port"].(float64)
	port := int(portF)
	if port == 0 {
		port = 5000
	}
	listenPublic, _ := cfg["listen_public"].(bool)
	host := "127.0.0.1"
	if listenPublic {
		host = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	// Log file for ongoing diagnostics. Writes to logFile and os.Stderr,
	// keeping os.Stdout strictly clean for MCP JSON-RPC protocol.
	logFile := openLogFile()
	logPath := ""
	if logFile != nil {
		logPath = logFile.Name()
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	}

	isPiped := isStdinPiped()

	if !isPiped {
		// Interactive / Double-click execution: display banner
		fmt.Printf("============================================================\n")
		fmt.Printf(" WOS & CNKI MCP Server  v1.0.0\n")
		fmt.Printf(" 功能: Web of Science 检索 / CNKI 检索·免验证摘要\n")
		fmt.Printf(" 后台维护: 每 2 小时自动校验并刷新 WOS / CNKI 登录 Cookie\n")
		fmt.Printf(" ------------------------------------------------------------\n")
		fmt.Printf(" SSE 端点 : http://%s/sse\n", addr)
		if logPath != "" {
			fmt.Printf(" 日志文件 : %s\n", logPath)
		} else {
			fmt.Printf(" 日志文件 : (打开失败, 输出保持到当前终端)\n")
		}
		fmt.Printf(" ------------------------------------------------------------\n")
		fmt.Printf(" 本窗口将在 5 秒后自动关闭, 服务转为后台静默运行。\n")
		fmt.Printf(" 需要停止时, 请在任务管理器中结束 wos_mcp_go.exe\n")
		fmt.Printf("============================================================\n")

		// Close the console window 5s after launch so the server keeps running
		// silently in the background (Windows only; no-op elsewhere).
		go func() {
			time.Sleep(5 * time.Second)
			detachConsole()
		}()
	} else {
		log.Printf("[Main] Stdio pipe detected; running in Stdio MCP mode (SSE also active on %s)\n", addr)
	}

	go backgroundMaintainer()

	// Start SSE server with automatic retry/recovery loop
	go func() {
		for {
			log.Printf("[SSE] Starting SSE server on %s\n", addr)
			sseServer := server.NewSSEServer(setupServer())
			if err := sseServer.Start(addr); err != nil {
				log.Printf("[SSE] Server error: %v. Restarting in 2s...\n", err)
				time.Sleep(2 * time.Second)
			} else {
				break
			}
		}
	}()

	if isPiped {
		// Stdio MCP mode: run Stdio server on main thread
		log.Println("[Stdio] Starting MCP server on stdio...")
		if err := server.ServeStdio(setupServer()); err != nil {
			log.Printf("[Stdio] Stdio session ended: %v\n", err)
		}
	} else {
		// Standalone execution: wait for exit signal
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		log.Println("[Main] Server shutting down.")
	}
}

// openLogFile opens (or truncates) the debug log file in overwrite mode.
// Log output is written to logFile and os.Stderr, keeping os.Stdout clean for MCP JSON-RPC.
func openLogFile() *os.File {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "debug.log"))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "debug.log"))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), "wos_mcp_go_debug.log"))

	for _, p := range candidates {
		if dir := filepath.Dir(p); dir != "" {
			_ = os.MkdirAll(dir, 0755)
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err == nil {
			return f
		}
	}
	return nil
}

func searchLiteratureHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}
	query, ok := args["query"].(string)
	if !ok {
		return mcp.NewToolResultError("query is required"), nil
	}

	query, err := sanitizeQuery(query)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	limitF, ok := args["limit"].(float64)
	limit := 10
	if ok && limitF > 0 {
		limit = int(limitF)
		if limit > 100 {
			limit = 100
		}
	}

	log.Printf("[MCP] search_literature called: query=%q, limit=%d\n", query, limit)

	sid, cookies, err := ensureWosSession()
	if err != nil {
		log.Printf("[MCP Error] search_literature ensureWosSession failed: %v\n", err)
		return mcp.NewToolResultError(fmt.Sprintf("Session error: %v", err)), nil
	}

	url := fmt.Sprintf("https://www.webofscience.com/api/wosnx/core/runQuerySearch?SID=%s", sid)
	payload := map[string]interface{}{
		"product":     "ALLDB",
		"searchMode":  "general_semantic",
		"viewType":    "search",
		"serviceMode": "summary",
		"search": map[string]interface{}{
			"mode":        "general_semantic",
			"database":    "ALLDB",
			"disableEdit": false,
			"query":       []map[string]interface{}{{"rowText": fmt.Sprintf("TS=(%s)", query)}},
			"display":     map[string]interface{}{"key": "nlp", "params": map[string]interface{}{"input": query, "query_type": "Single-Term Concept"}},
			"blending":    "blended",
			"count":       limit,
		},
		"retrieve": map[string]interface{}{
			"first":     1,
			"count":     limit,
			"history":   true,
			"jcr":       true,
			"sort":      "relevance",
			"analyzes":  []string{"TP.Value.6"},
			"trueCount": false,
			"locale":    "en",
		},
	}

	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(data))
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", "https://www.webofscience.com")
	req.Header.Set("Referer", "https://www.webofscience.com/wos/alldb/smart-search")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Accept", "application/x-ndjson, application/json, text/plain, */*")
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	parsedData, err := parseWosResponse(body)
	if err != nil {
		return mcp.NewToolResultError("Failed to parse WOS response"), nil
	}

	var recordsData map[string]interface{}
	var searchInfo map[string]interface{}
	for _, item := range parsedData {
		if key, ok := item["key"].(string); ok {
			if key == "records" {
				if pl, ok := item["payload"].(map[string]interface{}); ok {
					recordsData = pl
				}
			} else if key == "searchInfo" {
				if pl, ok := item["payload"].(map[string]interface{}); ok {
					searchInfo = pl
				}
			}
		}
	}

	totalResults := 0
	if searchInfo != nil {
		if tr, ok := searchInfo["RecordsFound"].(float64); ok {
			totalResults = int(tr)
		}
	}

	if len(recordsData) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("Found 0 records. Total reported: %d", totalResults)), nil
	}

	out := fmt.Sprintf("Found **%d** results in WoS.\n\n| # | Title | Authors | Source | Year | DOI | WoS ID |\n|---|-------|---------|--------|------|-----|--------|\n", totalResults)

	i := 1
	for _, recI := range recordsData {
		rec, ok := recI.(map[string]interface{})
		if !ok {
			continue
		}

		title := ""
		if titles, ok := rec["titles"].(map[string]interface{}); ok {
			if item, ok := titles["item"].(map[string]interface{}); ok {
				if en, ok := item["en"].([]interface{}); ok && len(en) > 0 {
					if enM, ok := en[0].(map[string]interface{}); ok {
						title, _ = enM["title"].(string)
					}
				}
			}
		}

		authors := ""
		if names, ok := rec["names"].(map[string]interface{}); ok {
			if author, ok := names["author"].(map[string]interface{}); ok {
				if en, ok := author["en"].([]interface{}); ok {
					for _, aI := range en {
						if aM, ok := aI.(map[string]interface{}); ok {
							wosStd, _ := aM["wos_standard"].(string)
							authors += wosStd + "; "
						}
					}
				}
			}
		}

		source := ""
		if titles, ok := rec["titles"].(map[string]interface{}); ok {
			if src, ok := titles["source"].(map[string]interface{}); ok {
				if en, ok := src["en"].([]interface{}); ok && len(en) > 0 {
					if enM, ok := en[0].(map[string]interface{}); ok {
						source, _ = enM["title"].(string)
					}
				}
			}
		}

		year := ""
		if pub, ok := rec["pub_info"].(map[string]interface{}); ok {
			if y, ok := pub["pubyear"].(float64); ok {
				year = fmt.Sprintf("%.0f", y)
			} else if yStr, ok := pub["pubyear"].(string); ok {
				year = yStr
			}
		}

		doi, _ := rec["doi"].(string)
		wosId, _ := rec["colluid"].(string)

		out += fmt.Sprintf("| %d | %s | %s | %s | %s | %s | %s |\n", i, title, authors, source, year, doi, wosId)
		i++
	}

	return mcp.NewToolResultText(out), nil
}

func getWosPaperDetailsHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}
	wosId, ok := args["wos_id"].(string)
	if !ok {
		return mcp.NewToolResultError("wos_id is required"), nil
	}
	if !wosIDRe.MatchString(wosId) {
		return mcp.NewToolResultError("invalid wos_id format"), nil
	}

	log.Printf("[MCP] get_wos_paper_details called: wos_id=%s\n", wosId)

	sid, cookies, err := ensureWosSession()
	if err != nil {
		log.Printf("[MCP Error] get_wos_paper_details ensureWosSession failed: %v\n", err)
		return mcp.NewToolResultError(fmt.Sprintf("Session error: %v", err)), nil
	}

	url := fmt.Sprintf("https://www.webofscience.com/api/wosnx/core/runQuerySearch?SID=%s", sid)
	payload := map[string]interface{}{
		"product":     "WOSCC",
		"searchMode":  "general",
		"viewType":    "search",
		"serviceMode": "summary",
		"search": map[string]interface{}{
			"mode":     "general",
			"database": "WOSCC",
			"query":    []map[string]interface{}{{"rowField": "UT", "rowText": wosId}},
		},
		"retrieve": map[string]interface{}{
			"first":   1,
			"count":   1,
			"history": false,
			"jcr":     true,
			"sort":    "relevance",
			"locale":  "en",
		},
	}

	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(data))
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", "https://www.webofscience.com")
	req.Header.Set("Referer", "https://www.webofscience.com/wos/woscc/summary")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Accept", "application/x-ndjson, application/json, text/plain, */*")
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	parsedData, _ := parseWosResponse(body)

	var recordsData map[string]interface{}
	for _, item := range parsedData {
		if key, ok := item["key"].(string); ok && key == "records" {
			if pl, ok := item["payload"].(map[string]interface{}); ok {
				recordsData = pl
			}
		}
	}

	if len(recordsData) == 0 {
		return mcp.NewToolResultError("Record not found"), nil
	}

	resJson, _ := json.MarshalIndent(recordsData, "", "  ")
	return mcp.NewToolResultText(string(resJson)), nil
}
