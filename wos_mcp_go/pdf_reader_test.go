package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/mark3labs/mcp-go/mcp"
)

func paperTestEnvironment(t *testing.T) string {
	t.Helper()
	oldDir, oldConfig := downloadDir, ConfigPath
	root := t.TempDir()
	downloadDir = filepath.Join(root, "download")
	ConfigPath = filepath.Join(root, "config.json")
	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { downloadDir, ConfigPath = oldDir, oldConfig })
	return root
}

// Each page deliberately reuses F1 with a different ToUnicode mapping. This
// catches cross-page font-cache corruption as well as old regex extraction.
func fixturePDF(t *testing.T, pages ...string) []byte {
	t.Helper()
	objects := []string{"", ""}
	add := func(value string) int { objects = append(objects, value); return len(objects) }
	stream := func(value []byte, compressed bool) string {
		filter := ""
		if compressed {
			var buf bytes.Buffer
			zw := zlib.NewWriter(&buf)
			if _, err := zw.Write(value); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			value, filter = buf.Bytes(), " /Filter /FlateDecode"
		}
		return fmt.Sprintf("<< /Length %d%s >>\nstream\n%s\nendstream", len(value), filter, value)
	}
	var refs []string
	for _, text := range pages {
		var cmap, encoded strings.Builder
		runes := []rune(text)
		cmap.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n/CMapName /Test def\n/CMapType 2 def\n1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
		for start := 0; start < len(runes); start += 100 {
			end := min(start+100, len(runes))
			fmt.Fprintf(&cmap, "%d beginbfchar\n", end-start)
			for i := start; i < end; i++ {
				fmt.Fprintf(&cmap, "<%04X> <", i+1)
				for _, unit := range utf16.Encode([]rune{runes[i]}) {
					fmt.Fprintf(&cmap, "%04X", unit)
				}
				cmap.WriteString(">\n")
				fmt.Fprintf(&encoded, "%04X", i+1)
			}
			cmap.WriteString("endbfchar\n")
		}
		cmap.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend")
		cmapID := add(stream([]byte(cmap.String()), true))
		cidID := add("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /Test /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /DW 1000 >>")
		fontID := add(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /Test /Encoding /Identity-H /DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>", cidID, cmapID))
		contentID := add(stream([]byte("BT /F1 12 Tf 50 750 Td <"+encoded.String()+"> Tj ET"), true))
		pageID := add(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>", fontID, contentID))
		refs = append(refs, fmt.Sprintf("%d 0 R", pageID))
	}
	objects[0] = "<< /Type /Catalog /Pages 2 0 R >>"
	objects[1] = fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", len(pages), strings.Join(refs, " "))
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.7\n")
	offsets := []int{0}
	for i, object := range objects {
		offsets = append(offsets, pdf.Len())
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return pdf.Bytes()
}

func writeFixture(t *testing.T, path string, pages ...string) {
	t.Helper()
	if err := os.WriteFile(path, fixturePDF(t, pages...), 0600); err != nil {
		t.Fatal(err)
	}
}

func contentText(content *PaperContent) string {
	var text strings.Builder
	text.WriteString(content.Content)
	for _, page := range content.Pages {
		text.WriteString(page.Text)
	}
	return text.String()
}

func toolRequest(args map[string]any) mcp.CallToolRequest {
	r := mcp.CallToolRequest{}
	r.Params.Arguments = args
	return r
}

func TestPDFUnicodeContinuationAndPageRanges(t *testing.T) {
	root := paperTestEnvironment(t)
	path := filepath.Join(root, "中文 paper.pdf")
	pages := []string{"摘要中文ABC", "方法与实验结果123", "讨论局限和结论XYZ"}
	writeFixture(t, path, pages...)
	args := map[string]any{"file_path": path, "max_chars": 4}
	var got strings.Builder
	for calls := 0; ; calls++ {
		if calls > 30 {
			t.Fatal("continuation did not terminate")
		}
		response, err := readPDFHandler(context.Background(), toolRequest(args))
		if err != nil || response.IsError {
			t.Fatalf("read: %v, %+v", err, response)
		}
		content := response.StructuredContent.(*PaperContent)
		got.WriteString(contentText(content))
		if content.ReturnedChars > 4 || content.FullTextIncluded {
			t.Fatalf("incorrect coverage: %+v", content)
		}
		if !content.HasMore {
			break
		}
		if content.NextCall == nil || content.NextCall.Name != "read_pdf" || content.NextOffset <= content.Offset {
			t.Fatal("missing advancing continuation")
		}
		args = content.NextCall.Arguments
	}
	if got.String() != strings.Join(pages, "") {
		t.Fatalf("lost/duplicated Unicode: %q", got.String())
	}
	selected, err := ReadPaper(context.Background(), path, ReadOptions{StartPage: 2, EndPage: 2})
	if err != nil {
		t.Fatal(err)
	}
	if contentText(selected) != pages[1] || selected.RangeCoversDocument || selected.FullTextIncluded || selected.Pages[0].PageNumber != 2 {
		t.Fatalf("wrong range: %+v", selected)
	}
	full, err := ReadPaper(context.Background(), path, ReadOptions{})
	if err != nil || !full.FullTextIncluded || full.TotalPages != 3 {
		t.Fatalf("full text: %+v, %v", full, err)
	}
}

func TestPDFPathsAndRestrictions(t *testing.T) {
	root := paperTestEnvironment(t)
	path := filepath.Join(root, "外部 #1.pdf")
	writeFixture(t, path, "指定位置正文")
	uriPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		uriPath = "/" + uriPath
	}
	uri := (&url.URL{Scheme: "file", Path: uriPath}).String()
	for _, input := range []string{path, uri, `"` + path + `"`} {
		result, err := ReadPaper(context.Background(), input, ReadOptions{})
		if err != nil || contentText(result) != "指定位置正文" {
			t.Fatalf("path %q: %+v %v", input, result, err)
		}
	}
	if err := SaveConfig(Config{"listen_public": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPaper(context.Background(), path, ReadOptions{}); err == nil {
		t.Fatal("public listener allowed unrestricted local PDF")
	}
	if err := SaveConfig(Config{"listen_public": true, "local_pdf_roots": []string{root}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPaper(context.Background(), path, ReadOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"../escape.pdf", `..\escape.pdf`, "C:escape.pdf", "/escape.pdf"} {
		if _, err := sanitizeSandboxPath(input); err == nil {
			t.Errorf("allowed sandbox escape %q", input)
		}
	}
	if pathWithin(downloadDir, downloadDir+"-other/file.pdf") {
		t.Fatal("prefix sibling is not inside sandbox")
	}
	if _, _, err := EnsureSandboxFolder("topic/subtopic"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(downloadDir, "outside")
	if err := os.Symlink(root, link); err == nil {
		if _, err := sanitizeSandboxPath("outside/new/paper.pdf"); err == nil {
			t.Fatal("symlink escaped sandbox")
		}
	} else {
		t.Logf("symlink check unavailable: %v", err)
	}
}

func TestEmptyCorruptAndInvalidReads(t *testing.T) {
	paperTestEnvironment(t)
	path := filepath.Join(downloadDir, "mixed.pdf")
	writeFixture(t, path, "可读的一页", "")
	content, err := ReadPaper(context.Background(), "mixed.pdf", ReadOptions{})
	if err != nil || content.ExtractionComplete || content.FullTextIncluded || len(content.PagesWithoutText) != 1 {
		t.Fatalf("missing page incorrectly marked complete: %+v %v", content, err)
	}
	writeFixture(t, filepath.Join(downloadDir, "empty.pdf"), "")
	result, _ := readPDFHandler(context.Background(), toolRequest(map[string]any{"file_path": "empty.pdf"}))
	if !result.IsError || result.StructuredContent.(*PaperContent).ContentLevel != "partial_extraction" {
		t.Fatal("empty PDF reported as readable")
	}
	for _, data := range []string{"<html>login</html>", "%PDF-1.7\ncorrupt\n%%EOF"} {
		if err := os.WriteFile(filepath.Join(downloadDir, "broken.pdf"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPaper(context.Background(), "broken.pdf", ReadOptions{}); err == nil {
			t.Fatal("invalid PDF accepted")
		}
	}
	for _, extra := range []map[string]any{{"offset": -1}, {"max_chars": 0}, {"max_chars": 100001}, {"offset": 1.5}, {"start_page": 3}, {"end_page": 0}, {"max_chars": "20"}, {"offset": 500}} {
		extra["file_path"] = path
		result, _ := readPDFHandler(context.Background(), toolRequest(extra))
		if !result.IsError {
			t.Errorf("invalid arguments accepted: %+v", extra)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadPaper(ctx, path, ReadOptions{}); err == nil {
		t.Fatal("canceled request succeeded")
	}
}

func TestTextContinuationAndPDFCacheRefresh(t *testing.T) {
	paperTestEnvironment(t)
	textPath := filepath.Join(downloadDir, "notes.txt")
	if err := os.WriteFile(textPath, []byte("甲乙丙丁abc"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := ReadPaper(context.Background(), "notes.txt", ReadOptions{MaxChars: 3})
	if err != nil || first.Content != "甲乙丙" || first.NextOffset != 3 {
		t.Fatalf("text offset: %+v %v", first, err)
	}
	second, err := ReadPaper(context.Background(), "notes.txt", ReadOptions{Offset: first.NextOffset})
	if err != nil || second.Content != "丁abc" || second.HasMore {
		t.Fatalf("text continuation: %+v %v", second, err)
	}
	path := filepath.Join(downloadDir, "cached.pdf")
	writeFixture(t, path, "旧正文")
	if _, err := ReadPaper(context.Background(), path, ReadOptions{}); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, "新正文已更新")
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := ReadPaper(context.Background(), path, ReadOptions{})
			if err != nil || contentText(result) != "新正文已更新" {
				t.Errorf("stale cached text: %+v %v", result, err)
			}
		}()
	}
	wg.Wait()
}

func TestConfigPreservesReadingPolicy(t *testing.T) {
	paperTestEnvironment(t)
	cfg := Config{"listen_public": true, "local_pdf_roots": []string{"C:/papers"}, "unpaywall_email": "reader@example.org", "future_setting": "keep"}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded := LoadConfig()
	loaded["wos_sid"] = "refreshed"
	if err := SaveConfig(loaded); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["listen_public"] != true || saved["future_setting"] != "keep" || saved["unpaywall_email"] != cfg["unpaywall_email"] || len(getStringSlice(saved, "local_pdf_roots")) != 1 {
		t.Fatalf("configuration lost: %s", data)
	}
}
