package proxies

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jadylc/subs-check/config"
	"github.com/jadylc/subs-check/utils"
	"gopkg.in/yaml.v3"
)

const (
	subCacheDirName  = "cache"
	subCacheFileMode = 0644
	subCacheDirMode  = 0755
)

// subCacheFile 是单个订阅链接对应的缓存文件内容。
// 按链接维度存储:每个订阅链接一个文件,获取失败时可用该链接
// 自己的缓存节点参与后续检测,避免因一次网络抖动丢失全部节点。
type subCacheFile struct {
	// URL 是配置中的原始订阅链接(未经过 WarpUrl 展开,含时间占位符原样),
	// 这样同一链接跨轮次缓存键稳定。
	URL string `yaml:"url"`
	// Source 记录链接来源:本地配置为"本地配置",远程清单为其清单 URL。
	// 用于链接被移除时精准清理对应缓存。
	Source string `yaml:"source"`
	// Proxies 是该链接最近一次成功获取并解析出的节点列表。
	Proxies []map[string]any `yaml:"proxies"`
}

// subCacheDir 返回订阅节点缓存目录(<output>/cache)。
// 与 save/method.NewLocalSaver 使用相同的输出目录解析规则,
// 但 proxy 包不能反向依赖 save 包,故在此独立计算。
func subCacheDir() (string, error) {
	var outputPath string
	if config.GlobalConfig.OutputDir != "" {
		outputPath = config.GlobalConfig.OutputDir
	} else {
		basePath := utils.GetExecutablePath()
		if basePath == "" {
			return "", fmt.Errorf("获取可执行文件路径失败")
		}
		outputPath = filepath.Join(basePath, "output")
	}
	return filepath.Join(outputPath, subCacheDirName), nil
}

// subCachePath 返回某个订阅链接对应的缓存文件路径。
// 文件名取原始 URL 的 SHA-256,避免 URL 中的特殊字符破坏路径。
func subCachePath(rawURL string) (string, error) {
	dir, err := subCacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(rawURL))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".yaml"), nil
}

// SaveSubCache 保存某个订阅链接解析出的节点到缓存。
// 节点为空或 URL 为空时不写入;写入采用临时文件改名,避免进程中断留半截缓存。
func SaveSubCache(rawURL, source string, proxies []map[string]any) error {
	if len(proxies) == 0 || rawURL == "" {
		return nil
	}
	path, err := subCachePath(rawURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), subCacheDirMode); err != nil {
		return err
	}
	data, err := yaml.Marshal(subCacheFile{URL: rawURL, Source: source, Proxies: proxies})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, subCacheFileMode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // 改名失败时清理临时文件
		return err
	}
	return nil
}

// LoadSubCache 读取某个订阅链接的缓存节点。
// 无缓存、文件损坏或 URL 不匹配时返回 nil,由调用方自行兜底。
func LoadSubCache(rawURL string) []map[string]any {
	path, err := subCachePath(rawURL)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cf subCacheFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		slog.Warn("解析订阅缓存失败,已忽略", "url", rawURL, "err", err)
		return nil
	}
	if cf.URL != rawURL {
		// 缓存文件内容与请求的链接不一致(理论上只有哈希碰撞才会出现),丢弃
		slog.Warn("订阅缓存 URL 不匹配,已忽略", "want", rawURL, "got", cf.URL)
		return nil
	}
	return cf.Proxies
}

// CleanupSubCaches 清理已不存在订阅链接的缓存。
//
//   - activeLocal: 本轮仍存在的本地配置链接(配置中的原始 URL)
//   - activeRemote: 本轮成功获取的远程清单 URL -> 该清单返回的链接列表
//   - failedRemote: 本轮获取失败的远程清单 URL 集合(其子链接缓存保留,
//     避免清单网络抖动时误删缓存)
//
// 清理规则:
//  1. 来源为"本地配置"的缓存:链接不在 activeLocal 中则删除;
//  2. 来源为远程清单的缓存:清单本轮成功且链接不在其返回列表中则删除;
//     来源清单本轮获取失败则保留;
//  3. 来源清单已不在任何有效集合中(配置中已删除该清单)则删除。
func CleanupSubCaches(activeLocal []string, activeRemote map[string][]string, failedRemote map[string]bool) {
	dir, err := subCacheDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("读取订阅缓存目录失败", "dir", dir, "err", err)
		}
		return
	}

	localSet := make(map[string]bool, len(activeLocal))
	for _, u := range activeLocal {
		localSet[u] = true
	}

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cf subCacheFile
		if err := yaml.Unmarshal(data, &cf); err != nil || cf.URL == "" {
			// 无法识别的缓存文件,不属于任何有效链接,删除
			os.Remove(path)
			continue
		}

		keep := false
		if cf.Source == "本地配置" {
			keep = localSet[cf.URL]
		} else if failedRemote[cf.Source] {
			// 来源清单本轮不可用,无法判断其链接是否仍存在,保留缓存
			keep = true
		} else if urls, ok := activeRemote[cf.Source]; ok {
			// 清单本轮成功获取:链接必须仍在该清单返回列表中
			for _, u := range urls {
				if u == cf.URL {
					keep = true
					break
				}
			}
		}
		if !keep {
			slog.Info("清理已移除订阅链接的缓存", "url", cf.URL, "source", cf.Source)
			os.Remove(path)
		}
	}
}
