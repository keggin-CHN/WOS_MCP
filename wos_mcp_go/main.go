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
		"1.1.0",
		server.WithToolCapabilities(true),
		server.WithInstructions(academicReadingInstructions),
	)

	s.AddTool(mcp.NewTool("search_literature",
		mcp.WithDescription("检索 Web of Science 文献，返回题录/摘要，不是全文。研究、综述或分析任务应筛选相关文献后主动调用 download_literature，再按 next_call 连续阅读全文。"),
		mcp.WithString("query", mcp.Required(), mcp.Description("The search query string")),
		mcp.WithNumber("limit", mcp.Description("Number of results to return (default 10)")),
	), searchLiteratureHandler)

	s.AddTool(mcp.NewTool("get_wos_paper_details",
		mcp.WithDescription("获取 WoS 题录和摘要元数据，不含论文全文。需要分析方法、结果或局限时继续调用 download_literature。"),
		mcp.WithString("wos_id", mcp.Required(), mcp.Description("The Web of Science ID (e.g., WOS:000295471900004)")),
	), getWosPaperDetailsHandler)

	s.AddTool(mcp.NewTool("search_cnki",
		mcp.WithDescription("检索知网文献，返回题录/摘要，不是全文。研究、综述或分析任务应筛选相关论文后主动调用 download_cnki_paper，再按 next_call 连续阅读全文。"),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
		mcp.WithString("search_type", mcp.Description("搜索类型：主题/篇名/作者/关键词，默认主题")),
		mcp.WithNumber("limit", mcp.Description("Number of results to return")),
	), searchCnkiHandler)

	s.AddTool(mcp.NewTool("download_literature",
		mcp.WithDescription("获取并开始阅读文献全文：按 DOI/WoS ID 查找开放 PDF，按知网 URL/标题下载知网 PDF，或下载直链 PDF。默认立即返回带页码的正文分段；有 next_call 时继续读取直到 has_more=false。至少提供一种文献标识。"),
		mcp.WithString("doi_or_wosid", mcp.Description("DOI、doi.org URL 或 WOS: 开头的 ID")),
		mcp.WithString("doi", mcp.Description("DOI（doi_or_wosid 的别名）")),
		mcp.WithString("url", mcp.Description("知网详情页 URL 或 HTTP(S) PDF 直链")),
		mcp.WithString("title", mcp.Description("知网文献标题；提供 DOI/URL 时仅作为保存文件名")),
		mcp.WithString("query", mcp.Description("知网检索词，未提供其他标识时使用")),
		mcp.WithString("subfolder", mcp.Description("download 下的子目录，支持多级目录")),
		mcp.WithBoolean("extract_text", mcp.DefaultBool(true), mcp.Description("下载后立即阅读正文，默认 true；仅保存文件时才设 false")),
		mcp.WithInteger("max_chars", mcp.DefaultNumber(defaultReadChars), mcp.Min(1), mcp.Max(maxReadChars), mcp.Description("本次正文字符数上限，默认 20000；超出后返回精确续读参数")),
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
		mcp.WithDescription("获取知网论文题录和摘要，不含全文。url/title 至少一个；研读任务应继续调用 download_cnki_paper 获取正文。"),
		mcp.WithString("url", mcp.Description("CNKI paper URL")),
		mcp.WithString("title", mcp.Description("（可选）文章标题，提供时直接通过搜索获取详情，更可靠")),
	), getCnkiPaperDetailHandler)

	s.AddTool(mcp.NewTool("find_best_match",
		mcp.WithDescription("在知网搜索并返回候选题录/摘要，不是全文。选定文章后调用 download_cnki_paper 下载并阅读。"),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
		mcp.WithString("search_type", mcp.Description("搜索类型（主题/篇名/作者/关键词）")),
		mcp.WithNumber("limit", mcp.Description("结果数量 (default 5)")),
	), searchCnkiHandler)

	s.AddTool(mcp.NewTool("download_cnki_paper",
		mcp.WithDescription("下载知网 PDF 并立即开始阅读全文，默认返回带页码的正文分段和阅读进度。研究或综述时优先用于选定的论文；按 next_call 继续读取，不能把第一段当作全文。"),
		mcp.WithString("url", mcp.Description("知网文献详情页 URL (如 https://kns.cnki.net/kcms2/article/abstract?v=...)")),
		mcp.WithString("title", mcp.Description("文章标题（若未提供 URL，将自动按标题精确搜索并下载）")),
		mcp.WithString("query", mcp.Description("检索关键词（若未提供 URL/标题，将自动搜索第 1 篇匹配文献并下载）")),
		mcp.WithString("subfolder", mcp.Description("存放子文件夹名称（可选，如不填则存入当天日期目录或默认目录）")),
		mcp.WithBoolean("extract_text", mcp.DefaultBool(true), mcp.Description("默认 true：下载后立即返回正文；仅存档时设 false")),
		mcp.WithInteger("max_chars", mcp.DefaultNumber(defaultReadChars), mcp.Min(1), mcp.Max(maxReadChars), mcp.Description("每次正文字符数上限，默认 20000；超出部分通过 next_call 续读")),
	), downloadCnkiPaperHandler)

	s.AddTool(mcp.NewTool("list_downloaded_papers",
		mcp.WithDescription("查看 download 沙盒目录下的已下载文献与文件列表"),
		mcp.WithString("subfolder", mcp.Description("子文件夹路径（可选，留空查看整个 download 目录）")),
	), listDownloadedPapersHandler)

	s.AddTool(paperReadingTool("read_paper_content", "读取指定位置的 PDF 全文，支持本机绝对路径、file:// URI、download 相对路径；也可读取沙盒内 TXT/JSON/Markdown。返回页码、覆盖范围及 next_call；持续续读直到 has_more=false，并报告未提取的页面。"), readPaperContentHandler)
	s.AddTool(paperReadingTool("read_pdf", "直接读取用户指定本地路径的 PDF，无需先下载或移入 download。支持 Windows/Linux/macOS 绝对路径和 file:// URI，可按页选择或按字符连续阅读全文。路径属于 MCP 服务器所在机器。"), readPDFHandler)

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
		// MultiWriter stops at the first write error. Write the file first so
		// stderr becoming invalid after Windows FreeConsole cannot drop logs.
		log.SetOutput(io.MultiWriter(logFile, os.Stderr))
	}

	isPiped := isStdinPiped()

	if !isPiped {
		// Interactive / Double-click execution: display banner
		fmt.Printf("============================================================\n")
		fmt.Printf(" WOS & CNKI MCP Server  v1.1.0\n")
		fmt.Printf(" 功能: WoS / CNKI 检索·全文下载·本地 PDF 分页阅读\n")
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
			if logFile != nil {
				log.SetOutput(logFile)
			}
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
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(data))
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
	var next []ToolCall
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
		identifier := doi
		if identifier == "" {
			identifier = wosId
		}
		if identifier != "" {
			next = append(next, ToolCall{"download_literature", map[string]any{"doi_or_wosid": identifier}})
		}
		i++
	}

	return metadataResult(out, next...), nil
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
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(data))
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
	return metadataResult(string(resJson), ToolCall{"download_literature", map[string]any{"doi_or_wosid": wosId}}), nil
}
