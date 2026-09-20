package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestDownloadReadsAndContinuesByDefault(t *testing.T) {
	paperTestEnvironment(t)
	pdf := fixturePDF(t, "第一部分正文", "第二部分正文")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(pdf)
	}))
	defer server.Close()
	result, err := downloadLiteratureHandler(context.Background(), toolRequest(map[string]any{"url": server.URL + "/paper", "title": "A.1 论文", "subfolder": "topic/sub", "max_chars": 3}))
	if err != nil || result.IsError {
		t.Fatalf("download: %+v %v", result, err)
	}
	data := result.StructuredContent.(map[string]any)
	reading := data["reading"].(*PaperContent)
	if !reading.HasMore || contentText(reading) != "第一部" || data["next_call"] == nil {
		t.Fatalf("missing full text workflow: %+v", data)
	}
	paper := data["paper"].(DownloadedPaper)
	if !strings.HasPrefix(paper.RelativePath, "topic/sub/A.1 论文-") {
		t.Fatalf("unexpected file name: %+v", paper)
	}
	if saved, err := os.ReadFile(paper.FilePath); err != nil || !bytes.Equal(saved, pdf) {
		t.Fatalf("saved file differs: %v", err)
	}
	continued, _ := readPaperContentHandler(context.Background(), toolRequest(reading.NextCall.Arguments))
	if continued.IsError || contentText(continued.StructuredContent.(*PaperContent)) != "分正文" {
		t.Fatal("download continuation lost content")
	}
	result, _ = downloadLiteratureHandler(context.Background(), toolRequest(map[string]any{"url": server.URL, "extract_text": false}))
	data = result.StructuredContent.(map[string]any)
	if data["reading"] != nil || data["content_level"] != "downloaded_only" || data["next_call"] == nil {
		t.Fatal("extract_text=false was ignored")
	}
}

func TestRejectBadDownloadsAndCleanPartialFiles(t *testing.T) {
	paperTestEnvironment(t)
	valid := fixturePDF(t, "valid")
	for _, tt := range []struct {
		name, body string
		status     int
		length     int64
	}{
		{"html", "<html>login required</html>", 200, -1},
		{"caj", "CAJ binary data", 200, -1},
		{"truncated", "%PDF-1.7\npartial", 200, -1},
		{"http_error", string(valid), 403, -1},
		{"size_limit", string(valid), 200, maxPaperBytes + 1},
		{"short_body", string(valid), 200, int64(len(valid) + 50)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tt.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tt.body)), ContentLength: tt.length}
			if _, err := savePDFResponse(response, "bad", "paper"); err == nil {
				t.Fatal("invalid download accepted")
			}
		})
	}
	err := filepath.Walk(downloadDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			t.Errorf("failed download left a file: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := downloadPDFURL(ctx, http.DefaultClient, "http://127.0.0.1:1/paper.pdf", "", ""); err == nil {
		t.Fatal("canceled download succeeded")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestUnpaywallTriesAlternatePDFAndPreservesOptions(t *testing.T) {
	paperTestEnvironment(t)
	t.Setenv("UNPAYWALL_EMAIL", "reader@example.org")
	oldClient := literatureHTTPClient
	t.Cleanup(func() { literatureHTTPClient = oldClient })
	pdf := fixturePDF(t, "备用链接正文")
	var calls []string
	literatureHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Hostname())
		var body string
		switch req.URL.Hostname() {
		case "api.unpaywall.org":
			if req.URL.Query().Get("email") != "reader@example.org" || !strings.HasSuffix(req.URL.Path, "10.1234/test") {
				t.Errorf("wrong API request: %s", req.URL)
			}
			body = `{"title":"OA paper","best_oa_location":{"url_for_pdf":"https://bad.example/paper"},"oa_locations":[{"url_for_pdf":"https://bad.example/paper"},{"url_for_pdf":"https://good.example/paper"}]}`
		case "bad.example":
			body = "<html>login</html>"
		case "good.example":
			body = string(pdf)
		default:
			return nil, fmt.Errorf("unexpected network request: %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
	})}
	result, _ := downloadLiteratureHandler(context.Background(), toolRequest(map[string]any{"doi_or_wosid": "https://doi.org/10.1234/test", "title": "中文标题不应路由到知网", "subfolder": "oa", "max_chars": 2}))
	if result.IsError {
		t.Fatalf("OA failed: %+v", result)
	}
	data := result.StructuredContent.(map[string]any)
	if contentText(data["reading"].(*PaperContent)) != "备用" || !strings.HasPrefix(data["paper"].(DownloadedPaper).RelativePath, "oa/") {
		t.Fatalf("lost options: %+v", data)
	}
	if strings.Join(calls, ",") != "api.unpaywall.org,bad.example,good.example" {
		t.Fatalf("wrong candidate sequence: %v", calls)
	}
}

func TestMCPDiscoveryInitializeAndRead(t *testing.T) {
	root := paperTestEnvironment(t)
	path := filepath.Join(root, "protocol.pdf")
	writeFixture(t, path, "通过MCP协议读取全文")
	s := setupServer()
	rpc := func(method string, params any) map[string]any {
		request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		response := s.HandleMessage(context.Background(), request)
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["error"] != nil {
			t.Fatalf("RPC %s failed: %s", method, encoded)
		}
		return decoded["result"].(map[string]any)
	}
	initialized := rpc("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}})
	if !strings.Contains(initialized["instructions"].(string), "next_call") {
		t.Fatal("missing server reading instructions")
	}
	listed := rpc("tools/list", map[string]any{})
	tools := map[string]map[string]any{}
	for _, tool := range listed["tools"].([]any) {
		value := tool.(map[string]any)
		tools[value["name"].(string)] = value
	}
	for _, name := range []string{"read_pdf", "read_paper_content"} {
		schema := tools[name]["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		for _, property := range []string{"file_path", "start_page", "end_page", "offset", "max_chars"} {
			if props[property] == nil {
				t.Errorf("%s missing %s", name, property)
			}
		}
	}
	downloadSchema := tools["download_literature"]["inputSchema"].(map[string]any)
	for _, property := range []string{"url", "title", "doi", "subfolder", "extract_text"} {
		if downloadSchema["properties"].(map[string]any)[property] == nil {
			t.Errorf("download missing %s", property)
		}
	}
	read := rpc("tools/call", map[string]any{"name": "read_pdf", "arguments": map[string]any{"file_path": path}})
	if read["isError"] == true || read["structuredContent"].(map[string]any)["full_text_included"] != true {
		t.Fatalf("protocol read failed: %+v", read)
	}
	metadata := metadataResult("题录", ToolCall{"download_cnki_paper", map[string]any{"title": "论文"}})
	if metadata.StructuredContent.(map[string]any)["full_text_read"] != false || !strings.Contains(metadata.Content[0].(mcp.TextContent).Text, "download_cnki_paper") {
		t.Fatal("metadata did not direct full text reading")
	}
}
