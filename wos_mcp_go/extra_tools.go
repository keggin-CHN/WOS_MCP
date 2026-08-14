package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
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

// 1. download_literature
func downloadLiteratureHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}
	doiOrWosId, ok := args["doi_or_wosid"].(string)
	if !ok || strings.TrimSpace(doiOrWosId) == "" {
		return mcp.NewToolResultText("参数错误: 请输入 DOI 或 WoS ID"), nil
	}
	doiOrWosId = strings.TrimSpace(doiOrWosId)

	doi := doiOrWosId
	isDoi := strings.HasPrefix(doi, "10.")

	if !isDoi {
		// Attempt to get DOI from WOS
		sid, cookies, err := ensureWosSession()
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Error ensuring session: %v", err)), nil
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
				"query": []map[string]interface{}{
					{"rowField": "UT", "rowText": doiOrWosId},
				},
			},
			"retrieve": map[string]interface{}{
				"first":    1,
				"count":    1,
				"history":  false,
				"jcr":      true,
				"sort":     "relevance",
				"analyzes": []interface{}{},
				"locale":   "en",
			},
		}

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
			return mcp.NewToolResultText(fmt.Sprintf("WOS API Error: %v", err)), nil
		}
		defer respWos.Body.Close()
		bodyBytes, _ := io.ReadAll(respWos.Body)

		var parsedData []interface{}
		json.Unmarshal(bodyBytes, &parsedData)
		if len(parsedData) == 0 {
			// fallback to lines
			lines := strings.Split(string(bodyBytes), "\n")
			for _, line := range lines {
				if strings.TrimSpace(line) != "" {
					var item interface{}
					if json.Unmarshal([]byte(line), &item) == nil {
						parsedData = append(parsedData, item)
					}
				}
			}
		}

		foundDoi := ""
		for _, item := range parsedData {
			if dict, ok := item.(map[string]interface{}); ok {
				if key, ok := dict["key"].(string); ok && key == "records" {
					if payloadData, ok := dict["payload"].(map[string]interface{}); ok {
						for _, recVal := range payloadData {
							if rec, ok := recVal.(map[string]interface{}); ok {
								if d, ok := rec["doi"].(string); ok {
									foundDoi = d
								}
							}
						}
					}
				}
			}
		}

		if foundDoi == "" {
			msg := fmt.Sprintf("未能通过 WoS ID %s 查询到 DOI（可能不在核心合集或会话失效）。\n\n请提示用户：可手动访问 https://www.webofscience.com/wos/woscc/full-record/%s 查看。", doiOrWosId, doiOrWosId)
			return mcp.NewToolResultText(msg), nil
		}
		doi = foundDoi
	}

	// Query Unpaywall API
	unpaywallUrl := fmt.Sprintf("https://api.unpaywall.org/v2/%s?email=mcp-test@example.com", doi)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(unpaywallUrl)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Failed to query Unpaywall API: %v", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return mcp.NewToolResultText(fmt.Sprintf("API returned HTTP %d for DOI %s", resp.StatusCode, doi)), nil
	}

	body, _ := io.ReadAll(resp.Body)
	var data map[string]interface{}
	json.Unmarshal(body, &data)

	isOa, _ := data["is_oa"].(bool)
	oaLocations, _ := data["oa_locations"].([]interface{})
	var bestUrl string

	if isOa && len(oaLocations) > 0 {
		if firstLoc, ok := oaLocations[0].(map[string]interface{}); ok {
			if urlPdf, ok := firstLoc["url_for_pdf"].(string); ok && urlPdf != "" {
				bestUrl = urlPdf
			} else if url, ok := firstLoc["url"].(string); ok && url != "" {
				bestUrl = url
			}
		}
	}

	if bestUrl != "" {
		return mcp.NewToolResultText(fmt.Sprintf("Success! Open Access PDF available for DOI: %s\n\nDownload Link: %s", doi, bestUrl)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Sorry, no Open Access version found for DOI: %s", doi)), nil
}

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

	sid, cookies, err := ensureWosSession()
	if err != nil {
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

	// Simplified export format for demo (JSON stringified)
	return mcp.NewToolResultText(fmt.Sprintf("Export format %s requested.\n\nRaw WOS API Response (truncated):\n%s...", format, string(bodyBytes)[:500])), nil
}

// getCnkiPaperDetailHandler fetches article details via search (avoids clickWord captcha on abstract page).
func getCnkiPaperDetailHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}
	urlStr, ok := args["url"].(string)
	if !ok || urlStr == "" {
		return mcp.NewToolResultText("Error: url is required"), nil
	}

	// Also accept a title argument for direct title-based lookup
	titleArg, _ := args["title"].(string)

	client := NewCnkiClient()
	if err := client.ensureSession(); err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Session error: %v", err)), nil
	}

	// Strategy: Instead of fetching the clickWord-protected abstract page directly,
	// use the search interface (blockPuzzle-protected, which we CAN solve) to get article data.
	//
	// Step 1: Try to extract any available info from the URL itself
	// Step 2: Use title-based search to get full article details

	// If a title was provided directly, use it
	if titleArg != "" {
		return searchAndReturnDetail(client, titleArg, urlStr)
	}

	// Try to get the page - if it succeeds (session has verified token), parse it
	req, _ := http.NewRequest("GET", urlStr, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	req.Header.Set("Referer", "https://kns.cnki.net/kns8s/defaultresult/index")

	resp, err := client.session.Do(req)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Request failed: %v", err)), nil
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	finalURL := resp.Request.URL.String()

	// blocked tells us whether the article page is captcha-protected; declared
	// here so the "goto parsed" reuse path below doesn't jump over it.
	blocked := strings.Contains(finalURL, "verify/home") || resp.StatusCode == 403 ||
		strings.Contains(string(body), "/verify/cnki.ico")

	// If this article was already unlocked in this session, reuse the captchaId
	// that did it: "?captchaId=<id>" bypasses the captcha for that exact URL.
	unlockedCaptchaMu.Lock()
	knownCap, known := unlockedCaptcha[urlStr]
	unlockedCaptchaMu.Unlock()
	if known && knownCap != "" {
		req2, _ := http.NewRequest("GET", urlStr+"&captchaId="+knownCap, nil)
		req2.Header.Set("User-Agent", userAgent)
		req2.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
		req2.Header.Set("Referer", "https://kns.cnki.net/kns8s/defaultresult/index")
		if resp2, err2 := client.session.Do(req2); err2 == nil {
			b2, _ := io.ReadAll(resp2.Body)
			resp2.Body.Close()
			f2 := resp2.Request.URL.String()
			if resp2.StatusCode == 200 && !strings.Contains(f2, "verify/home") {
				body, finalURL = b2, f2
				resp = resp2
				blocked = false
				goto parsed
			}
		}
	}

	// If we got redirected to captcha (or a 403 with captcha message), solve the
	// captcha and re-fetch. Verified: CNKI re-checks the article page on EVERY
	// visit (no cookie unlock), and the solver only passes ~1/3 of challenges,
	// so loop with a fresh challenge each round until the page lets us through.
	for round := 0; blocked && round < 5; round++ {
		if round > 0 {
			// re-trigger to get a brand-new captchaId/challenge
			req, _ = http.NewRequest("GET", urlStr, nil)
			req.Header.Set("User-Agent", userAgent)
			req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
			req.Header.Set("Referer", "https://kns.cnki.net/kns8s/defaultresult/index")
			resp, err = client.session.Do(req)
			if err != nil {
				break
			}
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			finalURL = resp.Request.URL.String()
			if !strings.Contains(finalURL, "verify/home") {
				blocked = false
				break
			}
		}

		captchaSource := finalURL
		if !strings.Contains(captchaSource, "captchaType=") && resp.StatusCode == 403 {
			var jsonResp struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if jsonErr := json.Unmarshal(body, &jsonResp); jsonErr == nil && jsonResp.Code == -403 {
				captchaSource = jsonResp.Message
			} else {
				captchaSource = string(body)
			}
		}

		reType := regexp.MustCompile(`captchaType=([a-zA-Z]+)`)
		reIdent := regexp.MustCompile(`ident=([a-zA-Z0-9]+)`)
		reCap := regexp.MustCompile(`captchaId=([a-zA-Z0-9\-]+)`)
		mType := reType.FindStringSubmatch(captchaSource)
		mIdent := reIdent.FindStringSubmatch(captchaSource)
		mCap := reCap.FindStringSubmatch(captchaSource)

		solved := false
		if len(mType) > 1 && len(mIdent) > 1 && len(mCap) > 1 {
			captchaID := mCap[1]
			if mType[1] == "clickWord" {
				solved = solveClickWord(client.session, mIdent[1], captchaID, extractReturnURL(captchaSource))
			} else if mType[1] == "blockPuzzle" {
				solved = solveCaptcha(client.session, mIdent[1], captchaID)
			}
		}

		if !solved {
			continue
		}

		// Remember which captchaId unlocked this exact article URL so future
		// re-visits (same URL) can bypass the captcha with ?captchaId=.
		if len(mCap) > 1 {
			unlockedCaptchaMu.Lock()
			unlockedCaptcha[urlStr] = mCap[1]
			unlockedCaptchaMu.Unlock()
		}

		// Re-fetch the article page and try to read the abstract.
		refetch := func(attemptURL string) ([]byte, string, int, bool) {
			req2, _ := http.NewRequest("GET", attemptURL, nil)
			req2.Header.Set("User-Agent", userAgent)
			req2.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
			req2.Header.Set("Referer", "https://kns.cnki.net/kns8s/defaultresult/index")
			resp2, err2 := client.session.Do(req2)
			if err2 != nil {
				return nil, "", 0, false
			}
			b2, _ := io.ReadAll(resp2.Body)
			resp2.Body.Close()
			f2 := resp2.Request.URL.String()
			ok := resp2.StatusCode == 200 && !strings.Contains(f2, "verify/home")
			return b2, f2, resp2.StatusCode, ok
		}

		if b2, f2, s2, ok := refetch(urlStr); ok {
			body, finalURL = b2, f2
			resp = &http.Response{StatusCode: s2, Body: io.NopCloser(bytes.NewReader(b2))}
			blocked = false
		}
		// else: still blocked -> loop round 2+ re-triggers a fresh challenge.
	}

parsed:
	if blocked {
		// Can't access the abstract page directly - inform user
		return mcp.NewToolResultText(
			fmt.Sprintf("摘要页受到验证码保护，无法直接访问。\n"+
				"请使用 get_cnki_paper_detail 工具并额外传入 title 参数（文章标题），"+
				"或使用 search_cnki 工具按标题搜索以获取摘要。\n"+
				"URL: %s", urlStr),
		), nil
	}

	// Parse the page if we got it
	if resp.StatusCode == 200 {
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
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

		out := fmt.Sprintf("## %s\n", title)
		out += fmt.Sprintf("**作者:** %s\n", strings.Join(authors, ", "))
		out += fmt.Sprintf("**机构:** %s\n\n", strings.Join(institutions, ", "))
		out += fmt.Sprintf("**关键词:** %s\n\n", strings.Join(keywords, ", "))
		out += fmt.Sprintf("### 摘要\n%s\n\n", abstract)
		out += fmt.Sprintf("**URL:** %s\n", urlStr)

		return mcp.NewToolResultText(out), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Unexpected status %d from %s", resp.StatusCode, urlStr)), nil
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
				fmt.Printf("CNKI title-path: page loaded without abstract (%s), re-searching\n", result.URL)
			} else {
				// verify/home -> session flagged by frequency control. A fresh
				// LID via re-login is the most likely fix (handoff §4.1).
				if fresh, err := reloginFresh(); err == nil {
					client, sess = fresh, fresh.session
					fmt.Println("CNKI title-path: re-logged in, retrying with fresh LID")
				} else {
					fmt.Printf("CNKI title-path: re-login failed: %v\n", err)
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

	return mcp.NewToolResultText(out), nil
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
	limit := 10
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
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
	}
	return mcp.NewToolResultText(out.String()), nil
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
