package proxies

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jadylc/subs-check/config"
)

func withTempOutputDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := config.GlobalConfig.OutputDir
	config.GlobalConfig.OutputDir = dir
	t.Cleanup(func() { config.GlobalConfig.OutputDir = old })
	return dir
}

// ---- SaveSubCache / LoadSubCache 往返 ----

func TestSubCache_RoundTrip(t *testing.T) {
	withTempOutputDir(t)
	url := "https://example.com/sub?token=abc&t={Ymd}"
	proxies := []map[string]any{
		{"name": "n1", "server": "1.2.3.4", "port": 443},
		{"name": "n2", "server": "5.6.7.8", "port": 1080},
	}

	if err := SaveSubCache(url, "本地配置", proxies); err != nil {
		t.Fatalf("SaveSubCache: %v", err)
	}
	got := LoadSubCache(url)
	if len(got) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(got))
	}
	if got[0]["name"] != "n1" || got[1]["name"] != "n2" {
		t.Errorf("unexpected cache content: %v", got)
	}
}

func TestSubCache_EmptyOrMissing(t *testing.T) {
	withTempOutputDir(t)
	url := "https://example.com/sub"
	if got := LoadSubCache(url); got != nil {
		t.Fatalf("expected nil for missing cache, got %v", got)
	}
	// 空节点不写缓存: 写后仍读不到
	if err := SaveSubCache(url, "本地配置", nil); err != nil {
		t.Fatalf("SaveSubCache(nil): %v", err)
	}
	if got := LoadSubCache(url); got != nil {
		t.Fatalf("expected nil after empty save, got %v", got)
	}
	// 空 URL 不写、不 panic
	if err := SaveSubCache("", "本地配置", []map[string]any{{"name": "n"}}); err != nil {
		t.Fatalf("SaveSubCache(empty url): %v", err)
	}
}

func TestSubCache_CorruptedFileReturnsNil(t *testing.T) {
	withTempOutputDir(t)
	url := "https://example.com/corrupt"
	path, err := subCachePath(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("::: not yaml :::"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := LoadSubCache(url); got != nil {
		t.Fatalf("expected nil for corrupted cache, got %v", got)
	}
}

func TestSubCache_UrlMismatchReturnsNil(t *testing.T) {
	withTempOutputDir(t)
	// 模拟哈希碰撞/缓存内容与请求链接不一致: URL-A 的文件里存 URL-B 内容
	urlA := "https://example.com/a"
	urlB := "https://example.com/b"
	path, err := subCachePath(urlA)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	// SaveSubCache(urlB) 写的是 urlB 自己的路径,需要把内容复制到 urlA 路径
	if err := SaveSubCache(urlB, "本地配置", []map[string]any{{"name": "n", "server": "1.2.3.4"}}); err != nil {
		t.Fatal(err)
	}
	src, err := subCachePath(urlB)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	if got := LoadSubCache(urlA); got != nil {
		t.Fatalf("expected nil on url mismatch, got %v", got)
	}
}

// ---- CleanupSubCaches ----

func TestCleanupSubCaches_LocalLinkRemoved(t *testing.T) {
	withTempOutputDir(t)
	// 本地配置曾经有 a、b,本轮只剩 a
	if err := SaveSubCache("https://example.com/a", "本地配置", []map[string]any{{"name": "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSubCache("https://example.com/b", "本地配置", []map[string]any{{"name": "b"}}); err != nil {
		t.Fatal(err)
	}

	CleanupSubCaches([]string{"https://example.com/a"}, nil, nil)

	if LoadSubCache("https://example.com/a") == nil {
		t.Fatal("cache of still-active link a should be kept")
	}
	if LoadSubCache("https://example.com/b") != nil {
		t.Fatal("cache of removed local link b should be deleted")
	}
}

func TestCleanupSubCaches_RemoteListNoLongerContains(t *testing.T) {
	withTempOutputDir(t)
	listURL := "https://example.com/list"
	// 清单曾包含 a、b;本轮成功返回只剩 a
	if err := SaveSubCache("https://example.com/a?in=list", listURL, []map[string]any{{"name": "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSubCache("https://example.com/b?in=list", listURL, []map[string]any{{"name": "b"}}); err != nil {
		t.Fatal(err)
	}

	activeRemote := map[string][]string{listURL: {"https://example.com/a?in=list"}}
	CleanupSubCaches(nil, activeRemote, nil)

	if LoadSubCache("https://example.com/a?in=list") == nil {
		t.Fatal("cache of link still in list should be kept")
	}
	if LoadSubCache("https://example.com/b?in=list") != nil {
		t.Fatal("cache of link removed from list should be deleted")
	}
}

func TestCleanupSubCaches_RemoteListFailedKeepsCache(t *testing.T) {
	withTempOutputDir(t)
	listURL := "https://example.com/list"
	if err := SaveSubCache("https://example.com/x?in=list", listURL, []map[string]any{{"name": "x"}}); err != nil {
		t.Fatal(err)
	}

	// 清单本轮获取失败: 无法判断链接是否仍存在,保留缓存
	failedRemote := map[string]bool{listURL: true}
	CleanupSubCaches(nil, nil, failedRemote)

	if LoadSubCache("https://example.com/x?in=list") == nil {
		t.Fatal("cache should be kept when its source list failed this round")
	}
}

func TestCleanupSubCaches_RemoteListConfigRemoved(t *testing.T) {
	withTempOutputDir(t)
	listURL := "https://example.com/gone-list"
	if err := SaveSubCache("https://example.com/y?in=list", listURL, []map[string]any{{"name": "y"}}); err != nil {
		t.Fatal(err)
	}

	// 清单从配置中删除: 不在 activeRemote 也不在 failedRemote,缓存删除
	CleanupSubCaches(nil, nil, nil)

	if LoadSubCache("https://example.com/y?in=list") != nil {
		t.Fatal("cache should be deleted when its source list is no longer configured")
	}
}

func TestCleanupSubCaches_UnrecognizedFileDeleted(t *testing.T) {
	dir := withTempOutputDir(t)
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 写入一个无法解析的缓存文件
	if err := os.WriteFile(filepath.Join(cacheDir, "junk.yaml"), []byte(":::bad:::"), 0644); err != nil {
		t.Fatal(err)
	}

	CleanupSubCaches(nil, nil, nil)

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected junk cache file removed, got %d entries", len(entries))
	}
}
