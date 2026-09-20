package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mark3labs/mcp-go/mcp"
)

// unlockedCaptcha maps an article URL -> the clickWord captchaId that unlocked
// it. Verified: after web/check succeeds for article X, re-visiting X with
// "?captchaId=<that id>" bypasses the captcha (the id is bound to that URL).
// Re-visits of the SAME article skip the captcha; different articles still
// need their own challenge.
var (
	unlockedCaptchaMu sync.Mutex
	unlockedCaptcha   = map[string]string{}
)

// 2. export_wos_papers
func exportWosPapersHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}
	query, _ := args["query"].(string)
	limitF, ok := args["limit"].(float64)
	limit := 50
	if ok {
		limit = int(limitF)
	}
	yearRange, _ := args["year_range"].(string)
	docType, _ := args["doc_type"].(string)
	format, _ := args["format"].(string)
	if format == "" {
		format = "bibtex"
	}

	if limit > 100 {
		limit = 100
	}

	log.Printf("[MCP] export_wos_papers called: query=%q, limit=%d, format=%s\n", query, limit, format)

	sid, cookies, err := ensureWosSession()
	if err != nil {
		log.Printf("[MCP Error] export_wos_papers ensureWosSession failed: %v\n", err)
		return mcp.NewToolResultText(fmt.Sprintf("Error ensuring session: %v", err)), nil
	}

	rowText := fmt.Sprintf("TS=(%s)", query)
	if yearRange != "" {
		rowText += fmt.Sprintf(" AND PY=(%s)", yearRange)
	}
	if docType != "" {
		rowText += fmt.Sprintf(" AND DT=(%s)", docType)
	}

	payload := map[string]interface{}{
		"product":     "ALLDB",
		"searchMode":  "general_semantic",
		"viewType":    "search",
		"serviceMode": "summary",
		"search": map[string]interface{}{
			"mode":        "general_semantic",
			"database":    "ALLDB",
			"disableEdit": false,
			"query": []map[string]interface{}{
				{"rowText": rowText},
			},
			"display": map[string]interface{}{
				"key": "nlp",
				"params": map[string]interface{}{
					"input":      query,
					"query_type": "Single-Term Concept",
				},
			},
			"blending": "blended",
			"count":    limit,
		},
		"retrieve": map[string]interface{}{
			"first":     1,
			"count":     limit,
			"history":   false,
			"jcr":       false,
			"sort":      "relevance",
			"analyzes":  []interface{}{},
			"trueCount": false,
			"locale":    "en",
		},
		"eventMode": nil,
	}

	url := fmt.Sprintf("https://www.webofscience.com/api/wosnx/core/runQuerySearch?SID=%s", sid)
	dataBytes, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(dataBytes))
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", "https://www.webofscience.com")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Accept", "application/x-ndjson, application/json, text/plain, */*")
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}
	respWos, err := httpClient.Do(req)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Error from Web of Science: %v", err)), nil
	}
	defer respWos.Body.Close()
	bodyBytes, _ := io.ReadAll(respWos.Body)

	respStr := string(bodyBytes)
	if len(respStr) > 500 {
		respStr = respStr[:500] + "..."
	}
	return mcp.NewToolResultText(fmt.Sprintf("Export format %s requested.\n\nRaw WOS API Response (truncated):\n%s", format, respStr)), nil
}

// getCnkiPaperDetailHandler fetches article details via search (avoids clickWord captcha on abstract page).
func getCnkiPaperDetailHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}
	urlStr, _ := args["url"].(string)
	titleArg, _ := args["title"].(string)
	if strings.TrimSpace(urlStr) == "" && strings.TrimSpace(titleArg) == "" {
		return mcp.NewToolResultError("请提供 url 或 title"), nil
	}

	client := NewCnkiClient()
	if err := client.ensureSession(); err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Session error: %v", err)), nil
	}

	// If a title was provided directly, use it
	if titleArg != "" {
		return searchAndReturnDetail(client, titleArg, urlStr)
	}

	bodyStr, err := client.FetchDetailPageHTML(urlStr)
	if err != nil {
		// Can't access abstract page directly - inform user to provide title
		return mcp.NewToolResultText(
			fmt.Sprintf("获取详情页失败或受验证码保护: %v\n"+
				"建议：请在调用 get_cnki_paper_detail 时额外传入 title 参数（文章标题），"+
				"或使用 search_cnki 工具按标题搜索以获取摘要。\n"+
				"URL: %s", err, urlStr),
		), nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(bodyStr))
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Failed to parse HTML: %v", err)), nil
	}

	title := "Unknown Title"
	if s := doc.Find(".wx-tit h1"); s.Length() > 0 {
		title = strings.TrimSpace(s.Text())
	} else if s := doc.Find("h1.title"); s.Length() > 0 {
		title = strings.TrimSpace(s.Text())
	}

	var authors []string
	doc.Find("h3.author span a, .author a").Each(func(_ int, s *goquery.Selection) {
		authors = append(authors, strings.TrimSpace(s.Text()))
	})

	var institutions []string
	doc.Find("h3.orgn span a, .orgn a").Each(func(_ int, s *goquery.Selection) {
		institutions = append(institutions, strings.TrimSpace(s.Text()))
	})

	abstract := "No abstract"
	if s := doc.Find("#ChDivSummary"); s.Length() > 0 {
		abstract = strings.TrimSpace(s.Text())
	} else if s := doc.Find(".abstract-text, .c-summary"); s.Length() > 0 {
		abstract = strings.TrimSpace(s.Text())
	}

	var keywords []string
	doc.Find("p.keywords a, .keywords a").Each(func(_ int, s *goquery.Selection) {
		kw := strings.TrimSpace(s.Text())
		kw = strings.TrimRight(kw, ";")
		keywords = append(keywords, kw)
	})

	pdfLink, cajLink, _ := ExtractDownloadLinks(bodyStr)

	out := fmt.Sprintf("## %s\n", title)
	out += fmt.Sprintf("**作者:** %s\n", strings.Join(authors, ", "))
	out += fmt.Sprintf("**机构:** %s\n\n", strings.Join(institutions, ", "))
	out += fmt.Sprintf("**关键词:** %s\n\n", strings.Join(keywords, ", "))
	out += fmt.Sprintf("### 摘要\n%s\n\n", abstract)
	if pdfLink != "" {
		out += fmt.Sprintf("**PDF 下载链接**: %s\n", pdfLink)
	}
	if cajLink != "" {
		out += fmt.Sprintf("**CAJ 下载链接**: %s\n", cajLink)
	}
	out += fmt.Sprintf("**URL:** %s\n", urlStr)

	return metadataResult(out, ToolCall{"download_cnki_paper", map[string]any{"url": urlStr, "title": title}}), nil
}

// searchAndReturnDetail searches by title and returns formatted article detail.
// Strategy (handoff doc §4.1) to beat CNKI frequency control on the title path:
//   - the search-result URL (fresh, dynamically encrypted) is always preferred
//     over the caller's URL, because URLs that already triggered captcha stay
//     flagged even with a valid LID;
//   - detail-page GETs are serialized process-wide (≥cnkiDetailGap apart);
//   - when a fetch is blocked (verify/home), we force a re-login for a new LID
//     and re-search for a brand-new URL instead of hammering the same session.
func searchAndReturnDetail(client *CnkiClient, title, originalURL string) (*mcp.CallToolResult, error) {
	// sess is the session used for fetches; it is swapped to a fresh client's
	// session after a re-login.
	sess := client.session

	// fetchAbstract does one serialized detail-page GET and returns the abstract
	// text ("" if the page was blocked or carried no abstract) plus whether the
	// page was actually accessible (200, not redirected to verify/home).
	fetchAbstract := func(u string) (abstract string, accessible bool) {
		var b, fu string
		var ok bool
		serializedDetailFetch(func() {
			req, _ := http.NewRequest("GET", u, nil)
			req.Header.Set("User-Agent", userAgent)
			req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
			req.Header.Set("Referer", "https://kns.cnki.net/kns8s/defaultresult/index")
			resp, err := sess.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			b, fu = string(raw), resp.Request.URL.String()
			ok = resp.StatusCode == 200 && !strings.Contains(fu, "verify/home")
		})
		if !ok {
			return "", false
		}
		doc, _ := goquery.NewDocumentFromReader(bytes.NewReader([]byte(b)))
		if s := doc.Find("#ChDivSummary"); s.Length() > 0 {
			return strings.TrimSpace(s.Text()), true
		}
		if s := doc.Find(".abstract-text, .c-summary"); s.Length() > 0 {
			return strings.TrimSpace(s.Text()), true
		}
		return "", true
	}

	var result *CnkiResult
	var lastErr error

	for round := 0; round < 3; round++ {
		if result == nil {
			r, e := client.GetArticleDetail(title)
			if e != nil {
				lastErr = e
				break
			}
			result = r
		}

		abs, accessible := fetchAbstract(result.URL)
		if abs != "" {
			result.Abstract = abs
			break
		}

		if round < 2 {
			if accessible {
				// Page loaded but had no abstract -> not a detail page. Re-search
				// for a fresh URL; re-login would not help here.
				log.Printf("CNKI title-path: page loaded without abstract (%s), re-searching\n", result.URL)
			} else {
				// verify/home -> session flagged by frequency control. A fresh
				// LID via re-login is the most likely fix (handoff §4.1).
				if fresh, err := reloginFresh(); err == nil {
					client, sess = fresh, fresh.session
					log.Println("CNKI title-path: re-logged in, retrying with fresh LID")
				} else {
					log.Printf("CNKI title-path: re-login failed: %v\n", err)
				}
			}
			result = nil // next round gets a brand-new search URL
			time.Sleep(2 * time.Second)
		}
	}

	if result == nil {
		if lastErr != nil {
			return mcp.NewToolResultText(fmt.Sprintf("搜索失败: %v", lastErr)), nil
		}
		return mcp.NewToolResultText("搜索失败: 未找到匹配文章"), nil
	}

	out := fmt.Sprintf("## %s\n", result.Title)
	out += fmt.Sprintf("**作者:** %s\n", result.Authors)
	out += fmt.Sprintf("**来源:** %s | **日期:** %s\n\n", result.Source, result.Date)
	if result.Abstract != "" {
		out += fmt.Sprintf("### 摘要\n%s\n\n", result.Abstract)
	} else {
		out += "### 摘要\n（未能获取摘要，请通过 URL 直接访问）\n\n"
	}
	if result.DOI != "" {
		out += fmt.Sprintf("**DOI:** %s\n", result.DOI)
	}
	out += fmt.Sprintf("**URL:** %s\n", result.URL)

	return metadataResult(out, ToolCall{"download_cnki_paper", map[string]any{"url": result.URL, "title": result.Title}}), nil
}

// searchCnkiHandler handles the search_cnki tool.
func searchCnkiHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultText("Error: query is required"), nil
	}
	searchType := "主题"
	if st, ok := args["search_type"].(string); ok && st != "" {
		searchType = st
	}
	limit, err := integerArg(args, "limit", 10, 1, 100)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	client := NewCnkiClient()
	results, err := client.Search(query, searchType, limit)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Search failed: %v", err)), nil
	}

	if len(results) == 0 {
		return mcp.NewToolResultText("No results found."), nil
	}

	var out strings.Builder
	next := make([]ToolCall, 0, len(results))
	fmt.Fprintf(&out, "共找到 %d 条结果:\n\n", len(results))
	for i, r := range results {
		fmt.Fprintf(&out, "[%d] **%s**\n", i+1, r.Title)
		fmt.Fprintf(&out, "    作者: %s\n", r.Authors)
		fmt.Fprintf(&out, "    来源: %s | 日期: %s\n", r.Source, r.Date)
		if r.Abstract != "" {
			fmt.Fprintf(&out, "    摘要: %s\n", r.Abstract)
		}
		if r.DOI != "" {
			fmt.Fprintf(&out, "    DOI: %s\n", r.DOI)
		}
		fmt.Fprintf(&out, "    URL: %s\n\n", r.URL)
		next = append(next, ToolCall{"download_cnki_paper", map[string]any{"url": r.URL, "title": r.Title}})
	}
	return metadataResult(out.String(), next...), nil
}

// 5. format_citation
func formatCitationHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}
	title, _ := args["title"].(string)
	authors, _ := args["authors"].(string)
	source, _ := args["source"].(string)
	year, _ := args["year"].(string)

	return mcp.NewToolResultText(fmt.Sprintf("[%d] %s. %s[J]. %s, %s.", 1, authors, title, source, year)), nil
}

// 6. export_cnki_papers
func exportCnkiPapersHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}
	papersJson, _ := args["papers_json"].(string)
	return mcp.NewToolResultText(fmt.Sprintf("Exported:\n%s", papersJson)), nil
}

// downloadCnkiPaperHandler downloads a paper from CNKI given URL, title, or query.
func downloadCnkiPaperHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}

	urlStr, _ := args["url"].(string)
	title, _ := args["title"].(string)
	query, _ := args["query"].(string)
	subfolder, _ := args["subfolder"].(string)
	extractText, maxChars, err := downloadReadOptions(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strings.TrimSpace(urlStr) == "" && strings.TrimSpace(title) == "" && strings.TrimSpace(query) == "" {
		return mcp.NewToolResultError("请提供文献详情页 URL、文章标题(title)或检索词(query)"), nil
	}
	if urlStr != "" && !isCNKIURL(urlStr) {
		return mcp.NewToolResultError("url 必须是知网 HTTP(S) 文献详情页；PDF 直链请使用 download_literature"), nil
	}
	if _, err := sanitizeSandboxPath(subfolder); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	client := NewCnkiClient()
	if err := client.ensureSession(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("知网会话初始化失败: %v", err)), nil
	}

	searchKey := strings.TrimSpace(title)
	if searchKey == "" {
		searchKey = strings.TrimSpace(query)
	}

	paperTitle := searchKey
	paperAuthors := ""
	paperSource := ""
	paperDate := ""

	// If URL is not provided, search for the best match on CNKI first
	if strings.TrimSpace(urlStr) == "" {
		if searchKey == "" {
			return mcp.NewToolResultText("参数错误: 请提供文献详情页 URL、文章标题(title)或检索词(query)"), nil
		}

		log.Printf("[MCP] download_cnki_paper searching for %q\n", searchKey)
		results, err := client.Search(searchKey, "TI", 1)
		if err != nil || len(results) == 0 {
			results, err = client.Search(searchKey, "SU", 1)
		}

		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("知网检索失败: %v", err)), nil
		}
		if len(results) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("未在知网检索到与 %q 匹配的文献", searchKey)), nil
		}

		first := results[0]
		urlStr = first.URL
		paperTitle = first.Title
		paperAuthors = first.Authors
		paperSource = first.Source
		paperDate = first.Date
		log.Printf("[MCP] download_cnki_paper matched: title=%q, url=%s\n", paperTitle, urlStr)
	}

	// Download the paper
	relPath, absPath, actualFilename, sizeBytes, err := client.DownloadPaper(ctx, urlStr, subfolder, paperTitle)
	if err != nil {
		log.Printf("[MCP Error] download_cnki_paper failed: %v\n", err)
		return mcp.NewToolResultError(fmt.Sprintf("知网文献下载失败: %v\n\n文献 URL: %s", err, urlStr)), nil
	}

	return downloadedPaperResult(ctx, DownloadedPaper{
		FilePath: absPath, RelativePath: relPath, Filename: actualFilename,
		SizeBytes: sizeBytes, SourceURL: urlStr, Title: paperTitle,
		Authors: paperAuthors, Source: strings.TrimSpace(paperSource + " " + paperDate),
	}, extractText, maxChars), nil
}

// listDownloadedPapersHandler lists files in the download sandbox.
func listDownloadedPapersHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, _ := request.Params.Arguments.(map[string]interface{})
	subfolder := ""
	if args != nil {
		if sf, ok := args["subfolder"].(string); ok {
			subfolder = sf
		}
	}

	files, err := ListSandboxFiles(subfolder)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("读取沙盒目录失败: %v", err)), nil
	}

	if len(files) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("下载沙盒目录 (subfolder: %q) 下暂无任何文件。", subfolder)), nil
	}

	var out strings.Builder
	out.WriteString(fmt.Sprintf("### 下载沙盒文件列表 (共 %d 个项):\n\n", len(files)))
	out.WriteString("| 名称 | 类型 | 相对路径 | 大小 | 修改时间 |\n")
	out.WriteString("|---|---|---|---|---|\n")

	for _, f := range files {
		typ := "📄 文件"
		if f.IsDir {
			typ = "📁 目录"
		}
		modTimeStr := f.ModTime.Format("2006-01-02 15:04:05")
		out.WriteString(fmt.Sprintf("| `%s` | %s | `%s` | %s | %s |\n", f.Name, typ, f.RelativePath, f.SizeHuman, modTimeStr))
	}

	out.WriteString("\n💡 可通过 `read_paper_content` 工具传入 `file_path` 直接读取对应文件内容。")
	return mcp.NewToolResultText(out.String()), nil
}

// Resource Handlers
func getStatusResourceHandler(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	content := "{\"status\": \"running\", \"version\": \"pure_protocol_v1_go\"}"
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      "cnki://status",
			MIMEType: "application/json",
			Text:     content,
		},
	}, nil
}

func getSearchTypesResourceHandler(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	content := "{\"types\": [\"主题\", \"篇名\", \"作者\", \"关键词\", \"摘要\", \"全文\"]}"
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      "cnki://search-types",
			MIMEType: "application/json",
			Text:     content,
		},
	}, nil
}
