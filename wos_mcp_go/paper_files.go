package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxPaperBytes = 100 << 20

type SandboxFileInfo struct {
	Name         string    `json:"name"`
	RelativePath string    `json:"relative_path"`
	Size         int64     `json:"size_bytes"`
	SizeHuman    string    `json:"size_human"`
	ModTime      time.Time `json:"mod_time"`
	IsDir        bool      `json:"is_dir"`
}

func FormatFileSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// Resolve existing parents too, so a new download cannot escape via a symlink
// or Windows junction already present in the sandbox.
func resolveExistingPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if _, statErr := os.Lstat(abs); statErr == nil {
		return "", err // Dangling links are not ordinary new directories.
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return "", err
	}
	resolvedParent, err := resolveExistingPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(abs)), nil
}

func sanitizeSandboxPath(relPath string) (string, error) {
	if relPath == "" {
		relPath = "."
	}
	relPath = strings.ReplaceAll(relPath, "\\", "/")
	if !filepath.IsLocal(relPath) || strings.Contains(relPath, ":") {
		return "", fmt.Errorf("access denied: use a relative path inside download, without '..' or a drive prefix")
	}
	root, err := resolveExistingPath(downloadDir)
	if err != nil {
		return "", err
	}
	target, err := resolveExistingPath(filepath.Join(root, relPath))
	if err != nil {
		return "", err
	}
	if !pathWithin(root, target) {
		return "", fmt.Errorf("access denied: path or symlink escapes download sandbox")
	}
	return target, nil
}

func EnsureSandboxFolder(subfolder string) (string, string, error) {
	if strings.TrimSpace(subfolder) == "" {
		subfolder = time.Now().Format("2006-01-02")
	}
	abs, err := sanitizeSandboxPath(subfolder)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return "", "", fmt.Errorf("create download directory: %w", err)
	}
	root, err := resolveExistingPath(downloadDir)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(root, abs)
	return rel, abs, err
}

func ListSandboxFiles(subfolder string) ([]SandboxFileInfo, error) {
	target, err := sanitizeSandboxPath(subfolder)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return []SandboxFileInfo{}, nil
	}
	root, err := resolveExistingPath(downloadDir)
	if err != nil {
		return nil, err
	}
	results := []SandboxFileInfo{}
	err = filepath.Walk(target, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == target || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		results = append(results, SandboxFileInfo{info.Name(), filepath.ToSlash(rel), info.Size(), FormatFileSize(info.Size()), info.ModTime(), info.IsDir()})
		return nil
	})
	return results, err
}

// Relative paths retain sandbox semantics. Absolute paths/URIs are read-only
// and limited to PDFs; a public listener requires explicitly configured roots.
func resolvePaperPath(input string) (string, error) {
	path := strings.TrimSpace(input)
	if len(path) >= 2 && ((path[0] == '"' && path[len(path)-1] == '"') || (path[0] == '\'' && path[len(path)-1] == '\'')) {
		path = path[1 : len(path)-1]
	}
	if path == "" {
		return "", fmt.Errorf("file_path is required")
	}
	if strings.HasPrefix(strings.ToLower(path), "file:") {
		u, err := url.Parse(path)
		if err != nil || !strings.EqualFold(u.Scheme, "file") || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Opaque != "" {
			return "", fmt.Errorf("invalid file URI; use file:///C:/papers/paper.pdf or an absolute path")
		}
		path = u.Path
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			if runtime.GOOS != "windows" {
				return "", fmt.Errorf("remote file URI is unsupported; provide a path on the MCP server")
			}
			path = "//" + u.Host + path
		} else if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		path = filepath.FromSlash(path)
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("file URI must contain an absolute path on the MCP server")
		}
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(homeDir, path[2:])
	}
	if !filepath.IsAbs(path) {
		return sanitizeSandboxPath(path)
	}
	abs, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("open path on the MCP server: %w", err)
	}
	root, err := resolveExistingPath(downloadDir)
	if err != nil {
		return "", err
	}
	if pathWithin(root, abs) {
		return abs, nil
	}
	if !strings.EqualFold(filepath.Ext(abs), ".pdf") {
		return "", fmt.Errorf("only PDF files may be read outside the download sandbox")
	}
	cfg := LoadConfig()
	roots := getStringSlice(cfg, "local_pdf_roots")
	public, _ := cfg["listen_public"].(bool)
	if len(roots) == 0 && !public {
		return abs, nil
	}
	for _, allowed := range roots {
		if !filepath.IsAbs(allowed) {
			allowed = filepath.Join(filepath.Dir(ConfigPath), allowed)
		}
		canonical, err := filepath.EvalSymlinks(allowed)
		if err == nil && pathWithin(canonical, abs) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("PDF path is outside local_pdf_roots; configure its directory in config.json (public listeners default to the download sandbox)")
}
