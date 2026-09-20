package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

const (
	defaultReadChars = 20000
	maxReadChars     = 100000
	maxPDFPages      = 2000
	maxPDFTextBytes  = 32 << 20
)

type ReadOptions struct {
	StartPage int
	EndPage   int
	Offset    int
	MaxChars  int
}

type ToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type PaperPage struct {
	PageNumber int    `json:"page_number"`
	StartChar  int    `json:"start_char"`
	EndChar    int    `json:"end_char"`
	TotalChars int    `json:"total_chars"`
	Text       string `json:"text"`
}

type PaperContent struct {
	FilePath            string      `json:"file_path"`
	Format              string      `json:"format"`
	ContentLevel        string      `json:"content_level"`
	ExtractionMethod    string      `json:"extraction_method"`
	TotalPages          int         `json:"total_pages,omitempty"`
	StartPage           int         `json:"start_page,omitempty"`
	EndPage             int         `json:"end_page,omitempty"`
	Offset              int         `json:"offset"`
	NextOffset          int         `json:"next_offset"`
	TotalChars          int         `json:"total_chars"`
	ReturnedChars       int         `json:"returned_chars"`
	HasMore             bool        `json:"has_more"`
	RangeCoversDocument bool        `json:"range_covers_document"`
	FullTextIncluded    bool        `json:"full_text_included"`
	ExtractionComplete  bool        `json:"extraction_complete"`
	Pages               []PaperPage `json:"pages,omitempty"`
	Content             string      `json:"content,omitempty"`
	PagesWithoutText    []int       `json:"pages_without_text,omitempty"`
	Warnings            []string    `json:"warnings,omitempty"`
	NextCall            *ToolCall   `json:"next_call,omitempty"`
	ReadingNote         string      `json:"reading_note"`
}

type pdfDocument struct {
	pages    []string
	warnings []string
	bytes    int
}

type pdfCacheEntry struct {
	info os.FileInfo
	doc  *pdfDocument
	used time.Time
}

// Cache immutable text across continuation calls. Mutable PDF parser objects
// are never shared. Both the number of documents and total bytes are bounded.
var pdfTextCache = struct {
	sync.Mutex
	entries map[string]pdfCacheEntry
}{entries: make(map[string]pdfCacheEntry)}

func loadPDF(ctx context.Context, path string, info os.FileInfo) (doc *pdfDocument, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pdfTextCache.Lock()
	if entry, ok := pdfTextCache.entries[path]; ok && os.SameFile(entry.info, info) && entry.info.Size() == info.Size() && entry.info.ModTime().Equal(info.ModTime()) {
		entry.used = time.Now()
		pdfTextCache.entries[path] = entry
		pdfTextCache.Unlock()
		return entry.doc, nil
	}
	pdfTextCache.Unlock()
	defer func() {
		if p := recover(); p != nil {
			doc, err = nil, fmt.Errorf("PDF parser failed: %v; try an unencrypted PDF with a searchable text layer", p)
		}
	}()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	header := make([]byte, 1024)
	n, readErr := f.ReadAt(header, 0)
	if readErr != nil && readErr != io.EOF {
		return nil, readErr
	}
	if !bytes.Contains(header[:n], []byte("%PDF-")) {
		return nil, fmt.Errorf("file is not a PDF (it may be an HTML login page or CAJ file)")
	}
	r, err := pdf.NewReader(f, info.Size())
	if err != nil {
		return nil, fmt.Errorf("cannot parse PDF (damaged or password-protected): %w", err)
	}
	count := r.NumPage()
	if count < 1 || count > maxPDFPages {
		return nil, fmt.Errorf("PDF page count %d is outside the supported range 1-%d; split the document", count, maxPDFPages)
	}
	doc = &pdfDocument{pages: make([]string, count)}
	for i := 1; i <= count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		text, pageErr := extractPDFPage(r, i)
		if pageErr != nil {
			doc.warnings = append(doc.warnings, fmt.Sprintf("第 %d 页提取失败: %v", i, pageErr))
			continue
		}
		doc.pages[i-1] = text
		doc.bytes += len(text)
		if doc.bytes > maxPDFTextBytes {
			return nil, fmt.Errorf("extracted text exceeds %s; split the PDF", FormatFileSize(maxPDFTextBytes))
		}
	}
	pdfTextCache.Lock()
	defer pdfTextCache.Unlock()
	delete(pdfTextCache.entries, path)
	for {
		total := doc.bytes
		oldest := ""
		var oldestTime time.Time
		for key, entry := range pdfTextCache.entries {
			total += entry.doc.bytes
			if oldest == "" || entry.used.Before(oldestTime) {
				oldest, oldestTime = key, entry.used
			}
		}
		if len(pdfTextCache.entries) < 4 && total <= maxPDFTextBytes {
			break
		}
		delete(pdfTextCache.entries, oldest)
	}
	pdfTextCache.entries[path] = pdfCacheEntry{info: info, doc: doc, used: time.Now()}
	return doc, nil
}

func extractPDFPage(r *pdf.Reader, number int) (text string, err error) {
	defer func() {
		if p := recover(); p != nil {
			text, err = "", fmt.Errorf("page parser failed: %v", p)
		}
	}()
	// Font names (e.g. F1) are page-local. Reusing a name-based font cache across
	// pages can silently decode later pages using the wrong Unicode mapping.
	text, err = r.Page(number).GetPlainText(nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.ReplaceAll(strings.ToValidUTF8(text, "�"), "\x00", "")), nil
}

func ReadPaper(ctx context.Context, filePath string, opts ReadOptions) (*PaperContent, error) {
	if opts.StartPage == 0 {
		opts.StartPage = 1
	}
	if opts.MaxChars == 0 {
		opts.MaxChars = defaultReadChars
	}
	if opts.StartPage < 1 || opts.EndPage < 0 || opts.Offset < 0 || opts.MaxChars < 1 || opts.MaxChars > maxReadChars {
		return nil, fmt.Errorf("invalid read options: start_page >= 1, end_page >= 1 when set, offset >= 0, max_chars 1-%d", maxReadChars)
	}
	path, err := resolvePaperPath(filePath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxPaperBytes {
		return nil, fmt.Errorf("expected a regular file no larger than %s", FormatFileSize(maxPaperBytes))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &PaperContent{FilePath: path, Format: strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."), Offset: opts.Offset, ExtractionComplete: true}
	var texts []string
	if result.Format == "pdf" {
		doc, err := loadPDF(ctx, path, info)
		if err != nil {
			return nil, err
		}
		result.ExtractionMethod = "ledongthuc/pdf"
		result.TotalPages = len(doc.pages)
		if opts.EndPage == 0 {
			opts.EndPage = result.TotalPages
		}
		if opts.StartPage > opts.EndPage || opts.EndPage > result.TotalPages {
			return nil, fmt.Errorf("invalid page range %d-%d; PDF has %d pages", opts.StartPage, opts.EndPage, result.TotalPages)
		}
		result.StartPage, result.EndPage = opts.StartPage, opts.EndPage
		result.RangeCoversDocument = opts.StartPage == 1 && opts.EndPage == result.TotalPages
		result.Warnings = append(result.Warnings, doc.warnings...)
		result.Warnings = append(result.Warnings, "仅提取 PDF 文本层；图片、图表和公式的视觉内容未被读取，多栏版式可能影响文本顺序。")
		texts = doc.pages[opts.StartPage-1 : opts.EndPage]
		for i, text := range doc.pages {
			if strings.TrimSpace(text) == "" {
				result.PagesWithoutText = append(result.PagesWithoutText, i+1)
			}
		}
		if len(result.PagesWithoutText) > 0 {
			result.ExtractionComplete = false
			result.Warnings = append(result.Warnings, "存在无可提取文本的页面，可能是扫描页、纯图页、空白页或字体解码失败；需要 OCR 或视觉阅读，不能声称全文已读。")
		}
	} else {
		if result.Format != "txt" && result.Format != "json" && result.Format != "md" {
			return nil, fmt.Errorf("unsupported format .%s; use PDF, TXT, JSON or Markdown (convert CAJ to PDF first)", result.Format)
		}
		if opts.StartPage != 1 || opts.EndPage != 0 {
			return nil, fmt.Errorf("start_page/end_page apply only to PDFs; use offset for text files")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("text file must be UTF-8 encoded")
		}
		texts = []string{string(data)}
		result.ExtractionMethod, result.RangeCoversDocument = "utf-8", true
	}
	for _, text := range texts {
		result.TotalChars += utf8.RuneCountInString(text)
	}
	if opts.Offset > result.TotalChars {
		return nil, fmt.Errorf("offset %d exceeds selected text length %d", opts.Offset, result.TotalChars)
	}
	remaining, skip := opts.MaxChars, opts.Offset
	for i, text := range texts {
		length := utf8.RuneCountInString(text)
		if skip >= length {
			skip -= length
			continue
		}
		if remaining == 0 {
			break
		}
		runes := []rune(text)
		end := min(length, skip+remaining)
		chunk := string(runes[skip:end])
		if result.Format == "pdf" {
			result.Pages = append(result.Pages, PaperPage{opts.StartPage + i, skip, end, length, chunk})
		} else {
			result.Content = chunk
		}
		result.ReturnedChars += end - skip
		remaining -= end - skip
		skip = 0
	}
	result.NextOffset = result.Offset + result.ReturnedChars
	result.HasMore = result.NextOffset < result.TotalChars
	result.FullTextIncluded = result.RangeCoversDocument && result.ExtractionComplete && result.Offset == 0 && !result.HasMore
	result.ContentLevel = "full_text_chunk"
	if result.FullTextIncluded {
		result.ContentLevel = "full_text"
	} else if !result.ExtractionComplete {
		result.ContentLevel = "partial_extraction"
	}
	result.ReadingNote = "按页引用已读内容。has_more=true 时继续执行 next_call，直到所需全文分段全部读完；最后一段到达末尾不代表此前各段已读。range_covers_document=false 表示仅选择了部分页面。full_text_included 只表示本次响应包含全部可提取文本，不包括图片。"
	if result.HasMore {
		args := map[string]any{"file_path": path, "offset": result.NextOffset, "max_chars": opts.MaxChars}
		if result.Format == "pdf" {
			args["start_page"], args["end_page"] = opts.StartPage, opts.EndPage
		}
		result.NextCall = &ToolCall{Name: "read_paper_content", Arguments: args}
	}
	return result, nil
}
