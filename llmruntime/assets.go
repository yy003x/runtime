package llmruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yy003x/runtime/runtimeapi"
)

const defaultMaxAssetBytes int64 = 4 << 20
const defaultMaxAssetCacheEntries = 128

type assetResolver struct {
	roots    map[string]string
	maxBytes int64
	cacheMax int
	cacheMu  sync.Mutex
	cacheSeq uint64
	cache    map[string]assetCacheEntry
}

type assetCacheEntry struct {
	content  string
	size     int64
	modified int64
	accessed uint64
}

type resolvedAsset struct {
	content string
	path    string
}

func newAssetResolver(roots map[string]string, maxBytes int64) (*assetResolver, error) {
	return newAssetResolverWithCache(roots, maxBytes, defaultMaxAssetCacheEntries)
}

func newAssetResolverWithCache(roots map[string]string, maxBytes int64, cacheEntries int) (*assetResolver, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxAssetBytes
	}
	if cacheEntries <= 0 {
		cacheEntries = defaultMaxAssetCacheEntries
	}
	resolver := &assetResolver{
		roots: make(map[string]string, len(roots)), maxBytes: maxBytes,
		cacheMax: cacheEntries, cache: make(map[string]assetCacheEntry),
	}
	for name, root := range roots {
		name = strings.TrimSpace(name)
		if !validAssetRootName(name) {
			return nil, fmt.Errorf("asset root name %q is invalid", name)
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("asset root %s must be an absolute path", name)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			return nil, fmt.Errorf("resolve asset root %s: %w", name, err)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("asset root %s must be an existing directory", name)
		}
		resolver.roots[name] = resolved
	}
	return resolver, nil
}

func validAssetRootName(name string) bool {
	if name == "" {
		return false
	}
	for _, value := range name {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
			value >= '0' && value <= '9' || value == '.' || value == '_' || value == '-' {
			continue
		}
		return false
	}
	return true
}

func (r *assetResolver) read(ref runtimeapi.AssetRef) (resolvedAsset, error) {
	hasInline := ref.Inline != ""
	hasURI := strings.TrimSpace(ref.URI) != ""
	if hasInline == hasURI {
		return resolvedAsset{}, fmt.Errorf("asset requires exactly one of inline or uri")
	}
	if hasInline {
		if int64(len(ref.Inline)) > r.maxBytes {
			return resolvedAsset{}, fmt.Errorf("inline asset exceeds %d bytes", r.maxBytes)
		}
		if err := verifyDigest([]byte(ref.Inline), ref.SHA256); err != nil {
			return resolvedAsset{}, err
		}
		return resolvedAsset{content: ref.Inline}, nil
	}
	path, err := r.resolveURI(ref.URI)
	if err != nil {
		return resolvedAsset{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return resolvedAsset{}, fmt.Errorf("stat asset %s: %w", ref.URI, err)
	}
	if info.IsDir() {
		if strings.TrimSpace(ref.SHA256) != "" {
			return resolvedAsset{}, fmt.Errorf("sha256 requires an asset file, not a directory")
		}
		return resolvedAsset{path: path}, nil
	}
	if !info.Mode().IsRegular() {
		return resolvedAsset{}, fmt.Errorf("asset %s must be a regular file", ref.URI)
	}
	if info.Size() > r.maxBytes {
		return resolvedAsset{}, fmt.Errorf("asset %s exceeds %d bytes", ref.URI, r.maxBytes)
	}
	data, err := r.readFile(path, info)
	if err != nil {
		return resolvedAsset{}, fmt.Errorf("read asset %s: %w", ref.URI, err)
	}
	if err := verifyDigest(data, ref.SHA256); err != nil {
		return resolvedAsset{}, fmt.Errorf("asset %s: %w", ref.URI, err)
	}
	return resolvedAsset{content: string(data), path: path}, nil
}

func (r *assetResolver) readPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > r.maxBytes {
		return "", fmt.Errorf("asset entry must be a regular file no larger than %d bytes", r.maxBytes)
	}
	data, err := r.readFile(path, info)
	return string(data), err
}

func (r *assetResolver) readFile(path string, info os.FileInfo) ([]byte, error) {
	modified := info.ModTime().UnixNano()
	r.cacheMu.Lock()
	if cached, ok := r.cache[path]; ok && cached.size == info.Size() && cached.modified == modified {
		r.cacheSeq++
		cached.accessed = r.cacheSeq
		r.cache[path] = cached
		r.cacheMu.Unlock()
		return []byte(cached.content), nil
	}
	r.cacheMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	r.cacheSeq++
	r.cache[path] = assetCacheEntry{
		content: string(data), size: info.Size(), modified: modified, accessed: r.cacheSeq,
	}
	for len(r.cache) > r.cacheMax {
		oldestPath := ""
		oldestAccess := ^uint64(0)
		for candidate, entry := range r.cache {
			if entry.accessed < oldestAccess {
				oldestPath, oldestAccess = candidate, entry.accessed
			}
		}
		delete(r.cache, oldestPath)
	}
	return data, nil
}

func (r *assetResolver) resolveURI(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "asset" || parsed.Host == "" || parsed.User != nil ||
		parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("asset uri must use asset://<root>/<relative-path>")
	}
	root, ok := r.roots[parsed.Host]
	if !ok {
		return "", fmt.Errorf("unknown asset root %q", parsed.Host)
	}
	relative, err := url.PathUnescape(strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if err != nil || relative == "" || strings.ContainsRune(relative, '\x00') || filepath.IsAbs(relative) {
		return "", fmt.Errorf("asset uri path is invalid")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("asset uri must stay within root")
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", fmt.Errorf("resolve asset uri: %w", err)
	}
	within, err := filepath.Rel(root, candidate)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("asset uri resolves outside root")
	}
	return candidate, nil
}

func verifyDigest(data []byte, expected string) error {
	expected = strings.TrimSpace(strings.TrimPrefix(expected, "sha256:"))
	if expected == "" {
		return nil
	}
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("sha256 must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return fmt.Errorf("sha256 is invalid")
	}
	actual := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), expected) {
		return fmt.Errorf("sha256 mismatch")
	}
	return nil
}
