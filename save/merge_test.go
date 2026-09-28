package save

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jadylc/subs-check/check"
	"github.com/jadylc/subs-check/config"
	"gopkg.in/yaml.v3"
)

// 按链接缓存方案下不再有 all.yaml 全局增量合并:
// 失败链接的节点由 proxy 层按链接缓存兜底,
// 保存层只序列化本轮结果。因此 loadExistingAllProxies
// 及相关增量合并测试已删除,保留 0 节点轮次的跳过保存语义测试。

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

// 保存层只序列化本轮结果:上一轮已有节点不再被合并带回。
func TestSaveConfig_SerializesOnlyThisRound(t *testing.T) {
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

	// 第二轮: 只有 B 与 C。A 已不在本轮结果中(如对应链接被删除或获取失败),
	// 不再有全局增量合并,因此 A 不应出现在 all.yaml 中。
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
	if gotNames["A"] {
		t.Errorf("all.yaml should not contain removed node A, got %v", gotNames)
	}
	for _, want := range []string{"B", "C"} {
		if !gotNames[want] {
			t.Errorf("all.yaml missing %q, got %v", want, gotNames)
		}
	}
	if len(second) != 2 {
		t.Errorf("expected 2 proxies, got %d: %v", len(second), second)
	}
}

// 0 节点轮次跳过保存,不清空已有 all.yaml。
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

	// 下一轮 0 节点: 跳过保存, all.yaml 保留上一轮内容
	SaveConfig(nil)

	kept := readAllYamlProxies(t, dir)
	if len(kept) != 1 || kept[0]["name"] != "A" {
		t.Fatalf("expected existing node A preserved, got %v", kept)
	}
}
