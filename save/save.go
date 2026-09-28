package save

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/jadylc/subs-check/check"
	"github.com/jadylc/subs-check/config"
	proxyutils "github.com/jadylc/subs-check/proxy"
	"github.com/jadylc/subs-check/save/method"
	"github.com/jadylc/subs-check/utils"
	"gopkg.in/yaml.v3"
)

// SaveFunc 定义保存方法的函数签名
type SaveFunc func(data []byte, filename string) error

// SaveConfig 保存检查结果到本地，并可选保存到远程存储。
//
// 执行顺序很关键:
//  1. 先把 results 序列化保存到 history(此时 proxy["name"] 仍是原始名,
//     history 文件天然干净,keep-days 下次加载时不会累积标签)
//  2. 然后原地 mutate 每个 result.Proxy["name"] 为最终展示名
//     (调 check.RenderName 生成 base + 媒体标签 + 速度标签 + sub_tag)
//  3. 增量合并:加载本地已有的 all.yaml 节点,与本轮结果合并去重,
//     避免直接覆盖丢点(如某轮订阅获取失败或返回节点变少)。
//     合并后序列化成 all.yaml、mihomo.yaml、base64.txt
//     并写本地 / 远程 / SubStore
//
// 隐式契约: SaveConfig 调用后 results 视为已消费,调用方不应再读
// results[i].Proxy["name"](那已经是展示名,不是原始名)。
func SaveConfig(results []check.Result) {
	// ① 先写 history,此时 proxy["name"] 仍是原始值,history yaml 天然干净
	// 0 节点时不上历史快照(空快照没有意义),但已有节点合并逻辑仍会兜底保留存量
	if len(results) > 0 && config.GlobalConfig.KeepDays > 0 {
		historyYamlData, err := marshalProxies(results)
		if err != nil {
			slog.Error(fmt.Sprintf("序列化历史快照失败: %v", err))
		} else {
			SaveHistory(historyYamlData)
		}
	}

	// ② 原地 mutate:把每个 proxy 的 name 改成最终展示名
	for i := range results {
		if results[i].Proxy == nil {
			continue
		}
		results[i].Proxy["name"] = check.RenderName(results[i], true)
	}

	// ③ 增量合并:本轮结果 + 本地已有 all.yaml 节点,合并去重后统一序列化。
	// 这样即便本轮订阅失败/节点变少,也不会直接覆盖掉上一轮保存的好节点。
	merged := make([]map[string]any, 0, len(results))
	for _, r := range results {
		if r.Proxy != nil {
			merged = append(merged, r.Proxy)
		}
	}
	existing := loadExistingAllProxies()
	if len(existing) > 0 {
		merged = append(merged, existing...)
		merged = proxyutils.DeduplicateProxies(merged)
		slog.Info(fmt.Sprintf("增量合并节点: 本轮 %d 个, 已有 %d 个, 合并去重后 %d 个",
			len(results), len(existing), len(merged)))
	}

	if len(merged) == 0 {
		slog.Warn("本轮与已有均没有可保存的节点，跳过保存")
		return
	}

	allYamlData, err := yaml.Marshal(map[string]any{"proxies": merged})
	if err != nil {
		slog.Error(fmt.Sprintf("序列化代理数据失败: %v", err))
		return
	}

	// 保存 all.yaml 到本地
	if err := method.SaveToLocal(allYamlData, "all.yaml"); err != nil {
		slog.Error(fmt.Sprintf("保存all.yaml到本地失败: %v", err))
	}

	// 更新 SubStore 并获取衍生文件(mihomo.yaml / base64.txt)
	var mihomoData, base64Data []byte
	if config.GlobalConfig.SubStorePort != "" {
		utils.UpdateSubStore(allYamlData)
		mihomoData = fetchSubStoreData(
			fmt.Sprintf("%s/api/file/%s", utils.BaseURL, utils.MihomoName),
			"mihomo.yaml",
		)
		base64Data = fetchSubStoreData(
			fmt.Sprintf("%s/download/%s?target=V2Ray", utils.BaseURL, utils.SubName),
			"base64.txt",
		)
	}

	// 保存衍生文件到本地
	saveIfNotEmpty(method.SaveToLocal, mihomoData, "mihomo.yaml")
	saveIfNotEmpty(method.SaveToLocal, base64Data, "base64.txt")

	// 保存所有文件到远程(如果配置了远程保存方式)
	if config.GlobalConfig.SaveMethod == "local" {
		return
	}
	remoteSaver, err := newRemoteSaver()
	if err != nil {
		slog.Error(fmt.Sprintf("初始化远程保存方法(%s)失败: %v", config.GlobalConfig.SaveMethod, err))
		return
	}
	saveIfNotEmpty(remoteSaver, allYamlData, "all.yaml")
	saveIfNotEmpty(remoteSaver, mihomoData, "mihomo.yaml")
	saveIfNotEmpty(remoteSaver, base64Data, "base64.txt")
}

// marshalProxies 从检查结果中提取代理并序列化为 YAML
func marshalProxies(results []check.Result) ([]byte, error) {
	proxies := make([]map[string]any, 0, len(results))
	for _, result := range results {
		proxies = append(proxies, result.Proxy)
	}
	if len(proxies) == 0 {
		return nil, fmt.Errorf("没有可用的代理节点")
	}
	return yaml.Marshal(map[string]any{"proxies": proxies})
}

// fetchSubStoreData 从 SubStore API 获取数据
func fetchSubStoreData(url, name string) []byte {
	resp, err := http.Get(url)
	if err != nil {
		slog.Error(fmt.Sprintf("获取%s请求失败: %v", name, err))
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error(fmt.Sprintf("读取%s失败: %v", name, err))
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		slog.Error(fmt.Sprintf("获取%s失败, 状态码: %d, 错误信息: %s", name, resp.StatusCode, body))
		return nil
	}
	return body
}

// saveIfNotEmpty 当数据非空时执行保存
func saveIfNotEmpty(saver SaveFunc, data []byte, filename string) {
	if len(data) == 0 {
		return
	}
	if err := saver(data, filename); err != nil {
		slog.Error(fmt.Sprintf("保存%s到%s失败: %v", filename, config.GlobalConfig.SaveMethod, err))
	}
}

// loadExistingAllProxies 读取本地 output 目录下已保存的 all.yaml 节点列表。
// 用于增量更新:把上一轮已保存的节点与本轮结果合并去重,避免某轮订阅
// 获取失败/节点变少时直接覆盖掉存量节点。文件不存在或解析失败时返回 nil。
func loadExistingAllProxies() []map[string]any {
	saver, err := method.NewLocalSaver()
	if err != nil {
		slog.Error(fmt.Sprintf("获取本地保存器失败: %v", err))
		return nil
	}
	path := filepath.Join(saver.OutputPath, "all.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn(fmt.Sprintf("读取本地 all.yaml 失败: %v", err))
		}
		return nil
	}
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		slog.Warn(fmt.Sprintf("解析本地 all.yaml 失败: %v", err))
		return nil
	}
	return doc.Proxies
}

// newRemoteSaver 根据配置创建远程保存方法
func newRemoteSaver() (SaveFunc, error) {
	switch config.GlobalConfig.SaveMethod {
	case "r2":
		if err := method.ValiR2Config(); err != nil {
			return nil, fmt.Errorf("R2配置不完整: %w", err)
		}
		return method.UploadToR2Storage, nil
	case "gist":
		if err := method.ValiGistConfig(); err != nil {
			return nil, fmt.Errorf("Gist配置不完整: %w", err)
		}
		return method.UploadToGist, nil
	case "webdav":
		if err := method.ValiWebDAVConfig(); err != nil {
			return nil, fmt.Errorf("WebDAV配置不完整: %w", err)
		}
		return method.UploadToWebDAV, nil
	case "s3":
		if err := method.ValiS3Config(); err != nil {
			return nil, fmt.Errorf("S3配置不完整: %w", err)
		}
		return method.UploadToS3, nil
	default:
		return nil, fmt.Errorf("未知的保存方法: %s", config.GlobalConfig.SaveMethod)
	}
}
