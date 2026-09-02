package main

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SandboxFileInfo contains file metadata inside download sandbox.
type SandboxFileInfo struct {
	Name         string    `json:"name"`
	RelativePath string    `json:"relative_path"`
	Size         int64     `json:"size_bytes"`
	SizeHuman    string    `json:"size_human"`
	ModTime      time.Time `json:"mod_time"`
	IsDir        bool      `json:"is_dir"`
}

// FormatFileSize formats byte count to human-readable string.
func FormatFileSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// sanitizeSandboxPath validates that a relative path stays inside downloadDir.
func sanitizeSandboxPath(relPath string) (string, error) {
	cleanRel := filepath.Clean(relPath)
	if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
		// Clean leading slashes
		cleanRel = strings.TrimLeft(cleanRel, "\\/.")
	}
	absTarget := filepath.Join(downloadDir, cleanRel)
	absTarget = filepath.Clean(absTarget)

	absRoot, err := filepath.Abs(downloadDir)
	if err != nil {
		return "", err
	}
	absTargetClean, err := filepath.Abs(absTarget)
	if err != nil {
		return "", err
	}

	if !strings.HasPrefix(absTargetClean, absRoot) {
		return "", fmt.Errorf("access denied: path escapes download sandbox")
	}
	return absTargetClean, nil
}

// EnsureSandboxFolder creates and returns an isolated directory inside download sandbox.
func EnsureSandboxFolder(subfolder string) (string, string, error) {
	if subfolder == "" {
		subfolder = time.Now().Format("2006-01-02")
	}
	// Sanitize subfolder name
	subfolder = regexp.MustCompile(`[\\/:*?"<>|]`).ReplaceAllString(subfolder, "_")
	subfolder = strings.TrimSpace(subfolder)

	absPath, err := sanitizeSandboxPath(subfolder)
	if err != nil {
		return "", "", err
	}

	if err := os.MkdirAll(absPath, 0755); err != nil {
		return "", "", fmt.Errorf("failed to create directory: %w", err)
	}

	relPath, err := filepath.Rel(downloadDir, absPath)
	if err != nil {
		relPath = subfolder
	}
	return relPath, absPath, nil
}

// ListSandboxFiles lists all files and directories in download sandbox or subfolder.
func ListSandboxFiles(subfolder string) ([]SandboxFileInfo, error) {
	targetDir := downloadDir
	if subfolder != "" {
		p, err := sanitizeSandboxPath(subfolder)
		if err != nil {
			return nil, err
		}
		targetDir = p
	}

	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		return []SandboxFileInfo{}, nil
	}

	var results []SandboxFileInfo
	err := filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if path == targetDir {
			return nil
		}
		rel, err := filepath.Rel(downloadDir, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		results = append(results, SandboxFileInfo{
			Name:         info.Name(),
			RelativePath: rel,
			Size:         info.Size(),
			SizeHuman:    FormatFileSize(info.Size()),
			ModTime:      info.ModTime(),
			IsDir:        info.IsDir(),
		})
		return nil
	})

	return results, err
}

// ReadSandboxFile reads text content from a file in the download sandbox.
// If it's a PDF, attempts text extraction; otherwise reads as UTF-8 text.
func ReadSandboxFile(relPath string, maxChars int) (string, error) {
	absPath, err := sanitizeSandboxPath(relPath)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", relPath)
	}

	if maxChars <= 0 {
		maxChars = 20000
	}

	ext := strings.ToLower(filepath.Ext(absPath))
	if ext == ".pdf" {
		return ExtractTextFromPDF(absPath, maxChars)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}

	text := string(data)
	runes := []rune(text)
	if len(runes) > maxChars {
		return string(runes[:maxChars]) + fmt.Sprintf("\n\n...[已截断，共 %d 字符，显示前 %d 字符]", len(runes), maxChars), nil
	}
	return text, nil
}

// ExtractTextFromPDF extracts readable text from PDF streams.
func ExtractTextFromPDF(pdfPath string, maxChars int) (string, error) {
	data, err := os.ReadFile(pdfPath)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	streamRe := regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	matches := streamRe.FindAllSubmatch(data, -1)

	textTokenRe := regexp.MustCompile(`\((.*?)\)\s*Tj|\[(.*?)\]\s*TJ`)

	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		rawStream := m[1]
		decompressed := decompressFlate(rawStream)
		streamToSearch := rawStream
		if len(decompressed) > 0 {
			streamToSearch = decompressed
		}

		subMatches := textTokenRe.FindAllSubmatch(streamToSearch, -1)
		for _, sm := range subMatches {
			if len(sm) > 1 && len(sm[1]) > 0 {
				clean := cleanPDFString(string(sm[1]))
				if len(strings.TrimSpace(clean)) > 0 {
					out.WriteString(clean)
					out.WriteString(" ")
				}
			} else if len(sm) > 2 && len(sm[2]) > 0 {
				clean := extractTJArray(string(sm[2]))
				if len(strings.TrimSpace(clean)) > 0 {
					out.WriteString(clean)
					out.WriteString(" ")
				}
			}
		}

		if out.Len() >= maxChars*2 {
			break
		}
	}

	res := strings.TrimSpace(out.String())
	if len(res) == 0 {
		// Fallback: search for direct plain strings in PDF
		res = extractVisibleStrings(data, maxChars)
	}

	runes := []rune(res)
	if len(runes) > maxChars {
		return string(runes[:maxChars]) + fmt.Sprintf("\n\n...[PDF 文本已截断，共 %d 字符，显示前 %d 字符]", len(runes), maxChars), nil
	}
	if len(runes) == 0 {
		return fmt.Sprintf("PDF 文件大小: %s，未能直接提取出纯文本流（可能为扫描件图像或使用专用 CID 字体编码）。", FormatFileSize(int64(len(data)))), nil
	}
	return res, nil
}

func decompressFlate(data []byte) []byte {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	defer r.Close()
	decompressed, err := io.ReadAll(r)
	if err != nil {
		return nil
	}
	return decompressed
}

func cleanPDFString(s string) string {
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\r`, "\r")
	s = strings.ReplaceAll(s, `\t`, "\t")
	s = strings.ReplaceAll(s, `\(`, "(")
	s = strings.ReplaceAll(s, `\)`, ")")
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

func extractTJArray(tj string) string {
	var b strings.Builder
	inParen := false
	var cur strings.Builder
	for i := 0; i < len(tj); i++ {
		ch := tj[i]
		if ch == '(' && !inParen {
			inParen = true
			cur.Reset()
		} else if ch == ')' && inParen {
			inParen = false
			b.WriteString(cleanPDFString(cur.String()))
		} else if inParen {
			cur.WriteByte(ch)
		}
	}
	return b.String()
}

func extractVisibleStrings(data []byte, maxChars int) string {
	var b strings.Builder
	var cur []rune
	for _, r := range string(data) {
		if (r >= 32 && r <= 126) || (r >= 0x4e00 && r <= 0x9fa5) || r == '\n' || r == '\t' {
			cur = append(cur, r)
		} else {
			if len(cur) >= 4 {
				b.WriteString(string(cur))
				b.WriteString("\n")
			}
			cur = cur[:0]
		}
		if b.Len() >= maxChars*2 {
			break
		}
	}
	if len(cur) >= 4 {
		b.WriteString(string(cur))
	}
	return b.String()
}
