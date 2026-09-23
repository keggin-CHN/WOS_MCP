package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

var literatureHTTPClient = &http.Client{Timeout: 90 * time.Second}

func isCNKIURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "cnki.net" || strings.HasSuffix(host, ".cnki.net") || host == "cnki.com.cn" || strings.HasSuffix(host, ".cnki.com.cn")
}

func normalizeDOI(raw string) string {
	value := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(value), "doi:") {
		value = strings.TrimSpace(value[4:])
	}
	if u, err := url.Parse(value); err == nil && (strings.EqualFold(u.Hostname(), "doi.org") || strings.EqualFold(u.Hostname(), "dx.doi.org")) {
		value = strings.TrimPrefix(u.Path, "/")
	}
	return value
}

func downloadLiteratureHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, ok := request.Params.Arguments.(map[string]any)
	if !ok {
		return mcp.NewToolResultError("invalid arguments"), nil
	}
	extract, maxChars, err := downloadReadOptions(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	subfolder, _ := args["subfolder"].(string)
	if _, err := sanitizeSandboxPath(subfolder); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	identifier := ""
	for _, key := range []string{"doi_or_wosid", "doi", "url", "title", "query"} {
		if value, ok := args[key].(string); ok && strings.TrimSpace(value) != "" {
			identifier = strings.TrimSpace(value)
			break
		}
	}
	if identifier == "" {
		return mcp.NewToolResultError("请提供 DOI、WoS ID、知网标题/关键词或 PDF URL"), nil
	}
	normalized := normalizeDOI(identifier)
	isDOI := strings.HasPrefix(normalized, "10.") && strings.Contains(normalized, "/")
	u, _ := url.Parse(identifier)
	isURL := u != nil && (u.Scheme == "http" || u.Scheme == "https")
	if isCNKIURL(identifier) || (!isDOI && !isURL && !strings.HasPrefix(identifier, "WOS:") && !strings.Contains(identifier, "://")) {
		cnkiArgs := make(map[string]any, len(args))
		for key, value := range args {
			cnkiArgs[key] = value
		}
		if isCNKIURL(identifier) {
			cnkiArgs["url"] = identifier
		} else if _, exists := cnkiArgs["title"]; !exists {
			cnkiArgs["title"] = identifier
		}
		request.Params.Arguments = cnkiArgs
		return downloadCnkiPaperHandler(ctx, request)
	}
	if isURL && !isDOI {
		name, _ := args["title"].(string)
		paper, err := downloadPDFURL(ctx, literatureHTTPClient, identifier, subfolder, name)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("PDF 下载失败: %v", err)), nil
		}
		paper.Title = name
		return downloadedPaperResult(ctx, paper, extract, maxChars), nil
	}
	doi := normalized
	if !isDOI {
		if !wosIDRe.MatchString(identifier) {
			return mcp.NewToolResultError("无法识别文献标识；使用 DOI、WOS: ID、知网标题或 HTTP(S) PDF URL"), nil
		}
		doi, err = resolveWOSDOI(ctx, identifier)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	email := strings.TrimSpace(os.Getenv("UNPAYWALL_EMAIL"))
	if email == "" {
		email = strings.TrimSpace(getStr(LoadConfig(), "unpaywall_email"))
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email {
		return mcp.NewToolResultError("DOI 下载需要 Unpaywall 联系邮箱。请在 config.json 配置 unpaywall_email，或设置 UNPAYWALL_EMAIL；也可直接提供 PDF url。"), nil
	}
	apiURL := "https://api.unpaywall.org/v2/" + url.PathEscape(doi) + "?email=" + url.QueryEscape(email)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := literatureHTTPClient.Do(req)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Unpaywall 查询失败: %v", err)), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Unpaywall 返回 HTTP %d，未获取全文 (DOI %s)", resp.StatusCode, doi)), nil
	}
	type location struct {
		PDF string `json:"url_for_pdf"`
	}
	var data struct {
		Title     string     `json:"title"`
		Best      location   `json:"best_oa_location"`
		Locations []location `json:"oa_locations"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&data); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Unpaywall 响应无效: %v", err)), nil
	}
	locations := append([]location{data.Best}, data.Locations...)
	seen := map[string]bool{}
	var failures []string
	for _, loc := range locations {
		if loc.PDF == "" || seen[loc.PDF] {
			continue
		}
		seen[loc.PDF] = true
		name := data.Title
		if name == "" {
			name = doi
		}
		paper, err := downloadPDFURL(ctx, literatureHTTPClient, loc.PDF, subfolder, name)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", loc.PDF, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		paper.DOI, paper.Title = doi, data.Title
		return downloadedPaperResult(ctx, paper, extract, maxChars), nil
	}
	if len(failures) > 0 {
		return mcp.NewToolResultError("找到开放获取链接，但 PDF 下载失败，尚未阅读全文：\n" + strings.Join(failures, "\n")), nil
	}
	return mcp.NewToolResultError(fmt.Sprintf("DOI %s 暂无可下载的开放 PDF。可提供已有本地 PDF 路径调用 read_pdf；当前未获取全文。", doi)), nil
}

func resolveWOSDOI(ctx context.Context, id string) (string, error) {
	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]any{"wos_id": id}
	result, err := getWosPaperDetailsHandler(ctx, request)
	if err != nil {
		return "", err
	}
	if result.IsError {
		return "", fmt.Errorf("WoS DOI 查询失败: %v", result.Content)
	}
	metadata, ok := result.StructuredContent.(map[string]any)
	if !ok {
		return "", fmt.Errorf("WoS metadata response is unavailable")
	}
	raw, _ := metadata["content"].(string)
	var records map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		return "", fmt.Errorf("invalid WoS records: %w", err)
	}
	for _, record := range records {
		if doi, ok := record["doi"].(string); ok && doi != "" {
			return normalizeDOI(doi), nil
		}
	}
	return "", fmt.Errorf("WoS 文献 %s 未返回 DOI；可提供 PDF URL 或本地 PDF", id)
}

func downloadPDFURL(ctx context.Context, client *http.Client, rawURL, subfolder, name string) (DownloadedPaper, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return DownloadedPaper{}, fmt.Errorf("expected an HTTP(S) PDF URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return DownloadedPaper{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/pdf,application/octet-stream;q=0.9,*/*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return DownloadedPaper{}, err
	}
	defer resp.Body.Close()
	paper, err := savePDFResponse(resp, subfolder, name)
	paper.SourceURL = rawURL
	return paper, err
}

// Only publish a completed, PDF-shaped response. Temporary names also keep
// repeated downloads from overwriting an existing paper with the same title.
func savePDFResponse(resp *http.Response, subfolder, name string) (DownloadedPaper, error) {
	if resp.StatusCode != http.StatusOK {
		return DownloadedPaper{}, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxPaperBytes {
		return DownloadedPaper{}, fmt.Errorf("PDF exceeds %s", FormatFileSize(maxPaperBytes))
	}
	reader := bufio.NewReader(resp.Body)
	header, err := reader.Peek(512)
	if err != nil && err != io.EOF {
		return DownloadedPaper{}, err
	}
	if !bytes.HasPrefix(bytes.TrimSpace(header), []byte("%PDF-")) {
		return DownloadedPaper{}, fmt.Errorf("response is not PDF data (possibly an HTML login page, CAJ, or an access denial)")
	}
	if name == "" {
		name = ParseContentDispositionFilename(resp.Header.Get("Content-Disposition"))
	}
	name = sanitizeFilename(name)
	if ext := filepath.Ext(name); strings.EqualFold(ext, ".pdf") || strings.EqualFold(ext, ".caj") {
		name = strings.TrimSuffix(name, ext)
	}
	if name == "" {
		name = "paper"
	}
	stem := []rune(name)
	if len(stem) > 80 {
		stem = stem[:80]
	}
	relFolder, absFolder, err := EnsureSandboxFolder(subfolder)
	if err != nil {
		return DownloadedPaper{}, err
	}
	f, err := os.CreateTemp(absFolder, string(stem)+"-*.pdf.part")
	if err != nil {
		return DownloadedPaper{}, err
	}
	tempPath := f.Name()
	defer func() { f.Close(); os.Remove(tempPath) }()
	written, err := io.Copy(f, io.LimitReader(reader, maxPaperBytes+1))
	if err != nil {
		return DownloadedPaper{}, fmt.Errorf("incomplete download: %w", err)
	}
	if written > maxPaperBytes {
		return DownloadedPaper{}, fmt.Errorf("PDF exceeds %s", FormatFileSize(maxPaperBytes))
	}
	if resp.ContentLength >= 0 && written != resp.ContentLength {
		return DownloadedPaper{}, fmt.Errorf("incomplete download: received %d of %d bytes", written, resp.ContentLength)
	}
	logicalSize, err := pdfLogicalEnd(f, written)
	if err != nil {
		return DownloadedPaper{}, fmt.Errorf("incomplete PDF: %w", err)
	}
	if logicalSize != written {
		if err := f.Truncate(logicalSize); err != nil {
			return DownloadedPaper{}, fmt.Errorf("trim PDF trailing data: %w", err)
		}
		written = logicalSize
	}
	if err := f.Close(); err != nil {
		return DownloadedPaper{}, err
	}
	finalPath := strings.TrimSuffix(tempPath, ".part")
	if err := os.Rename(tempPath, finalPath); err != nil {
		return DownloadedPaper{}, err
	}
	return DownloadedPaper{FilePath: finalPath, RelativePath: filepath.ToSlash(filepath.Join(relFolder, filepath.Base(finalPath))), Filename: filepath.Base(finalPath), SizeBytes: written}, nil
}
