package save

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jadylc/subs-check/check"
	"github.com/jadylc/subs-check/config"
	"gopkg.in/yaml.v3"
)

// ---- loadExistingAllProxies ----

func TestLoadExistingAllProxies_MissingFile(t *testing.T) {
	dir := t.TempDir()
	oldOutputDir := config.GlobalConfig.OutputDir
	config.GlobalConfig.OutputDir = dir
	defer func() { config.GlobalConfig.OutputDir = oldOutputDir }()

	got := loadExistingAllProxies()
	if got != nil {
		t.Fatalf("expected nil for missing file, got %v", got)
	}
}

func TestLoadExistingAllProxies_ValidFile(t *testing.T) {
	dir := t.TempDir()
	oldOutputDir := config.GlobalConfig.OutputDir
	config.GlobalConfig.OutputDir = dir
	defer func() { config.GlobalConfig.OutputDir = oldOutputDir }()

	if err := os.WriteFile(filepath.Join(dir, "all.yaml"), []byte("proxies:\n  - name: n1\n    server: 1.2.3.4\n"), 0644); err != nil {
		t.Fatal(err)
	}

	got := loadExistingAllProxies()
	if len(got) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(got))
	}
	if got[0]["name"] != "n1" {
		t.Errorf("unexpected first proxy: %v", got[0])
	}
}

func TestLoadExistingAllProxies_InvalidFile(t *testing.T) {
	dir := t.TempDir()
	oldOutputDir := config.GlobalConfig.OutputDir
	config.GlobalConfig.OutputDir = dir
	defer func() { config.GlobalConfig.OutputDir = oldOutputDir }()

	if err := os.WriteFile(filepath.Join(dir, "all.yaml"), []byte("::: not yaml :::"), 0644); err != nil {
		t.Fatal(err)
	}

	got := loadExistingAllProxies()
	if got != nil {
		t.Fatalf("expected nil for invalid yaml, got %v", got)
	}
}

// ---- SaveConfig 增量合并 ----

// 构造一个最小可保存的 Result,避免依赖 RenderName 的媒体/测速字段
func mergeTestResult(name, server string, port int) check.Result {
	return check.Result{Proxy: map[string]any{
		"name":   name,
		"type":   "ss",
		"server": server,
		"port":   port,
	}}
}

func readAllYamlProxies(t *testing.T, dir string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "all.yaml"))
	if err != nil {
		t.Fatalf("read all.yaml: %v", err)
	}
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse all.yaml: %v", err)
	}
	return doc.Proxies
}

func TestSaveConfig_IncrementalMergeKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	oldOutputDir := config.GlobalConfig.OutputDir
	oldSaveMethod := config.GlobalConfig.SaveMethod
	oldSubStorePort := config.GlobalConfig.SubStorePort
	oldKeepDays := config.GlobalConfig.KeepDays
	config.GlobalConfig.OutputDir = dir
	config.GlobalConfig.SaveMethod = "local"
	config.GlobalConfig.SubStorePort = ""
	config.GlobalConfig.KeepDays = 0
	defer func() {
		config.GlobalConfig.OutputDir = oldOutputDir
		config.GlobalConfig.SaveMethod = oldSaveMethod
		config.GlobalConfig.SubStorePort = oldSubStorePort
		config.GlobalConfig.KeepDays = oldKeepDays
	}()

	// 第一轮: 节点 A、B
	SaveConfig([]check.Result{
		mergeTestResult("A", "1.2.3.4", 443),
		mergeTestResult("B", "5.6.7.8", 443),
	})
	first := readAllYamlProxies(t, dir)
	if len(first) != 2 {
		t.Fatalf("first round expected 2 proxies, got %d", len(first))
	}

	// 第二轮: 只有 B（与已有重复）以及新节点 C。
	// A 在第二轮已不在订阅结果中,但增量合并应保留 A。
	SaveConfig([]check.Result{
		mergeTestResult("B", "5.6.7.8", 443),
		mergeTestResult("C", "9.9.9.9", 443),
	})
	second := readAllYamlProxies(t, dir)

	gotNames := make(map[string]bool)
	for _, p := range second {
		if n, ok := p["name"].(string); ok {
			gotNames[n] = true
		}
	}
	for _, want := range []string{"A", "B", "C"} {
		if !gotNames[want] {
			t.Errorf("merged all.yaml missing %q, got %v", want, gotNames)
		}
	}
	if len(second) != 3 {
		t.Errorf("expected 3 deduplicated proxies, got %d: %v", len(second), second)
	}
}

func TestSaveConfig_NoOverwriteWhenEmptyRound(t *testing.T) {
	dir := t.TempDir()
	oldOutputDir := config.GlobalConfig.OutputDir
	oldSaveMethod := config.GlobalConfig.SaveMethod
	oldSubStorePort := config.GlobalConfig.SubStorePort
	oldKeepDays := config.GlobalConfig.KeepDays
	config.GlobalConfig.OutputDir = dir
	config.GlobalConfig.SaveMethod = "local"
	config.GlobalConfig.SubStorePort = ""
	config.GlobalConfig.KeepDays = 0
	defer func() {
		config.GlobalConfig.OutputDir = oldOutputDir
		config.GlobalConfig.SaveMethod = oldSaveMethod
		config.GlobalConfig.SubStorePort = oldSubStorePort
		config.GlobalConfig.KeepDays = oldKeepDays
	}()

	// 先保存一轮有节点的结果
	SaveConfig([]check.Result{
		mergeTestResult("A", "1.2.3.4", 443),
	})

	// 下一轮 0 节点: 不应清空 all.yaml,应保留上一轮节点
	SaveConfig(nil)

	kept := readAllYamlProxies(t, dir)
	if len(kept) != 1 || kept[0]["name"] != "A" {
		t.Fatalf("expected existing node A preserved, got %v", kept)
	}
}
