package proxies

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	u "net/url"
	"strings"
	"sync"
	"time"

	"github.com/jadylc/subs-check/config"
	"github.com/jadylc/subs-check/utils"
	"github.com/metacubex/mihomo/common/convert"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/samber/lo"
	"gopkg.in/yaml.v3"
)

type subEntry struct {
	url    string
	source string
}

// subUserinfoMap 缓存每个订阅链接的 Subscription-Userinfo 响应头
var subUserinfoMap sync.Map

// ResetSubUserinfoMap 清空订阅流量信息缓存
// 在每轮 GetProxies 开始时调用，避免已移除的订阅 URL 条目残留导致内存增长
func ResetSubUserinfoMap() {
	subUserinfoMap.Range(func(key, _ any) bool {
		subUserinfoMap.Delete(key)
		return true
	})
}

// GetSubUserinfo 返回订阅链接的流量信息字符串(原始 header 值)
func GetSubUserinfo(url string) string {
	if v, ok := subUserinfoMap.Load(url); ok {
		return v.(string)
	}
	return ""
}

func GetProxies() ([]map[string]any, error) {

	// 清空上一轮的订阅流量信息缓存，避免已移除的订阅 URL 条目残留
	ResetSubUserinfoMap()

	// 解析本地与远程订阅清单
	subUrls, localNum, remoteNum := resolveSubUrls()
	slog.Info("订阅链接数量", "本地", localNum, "远程", remoteNum, "总计", len(subUrls))

	if len(config.GlobalConfig.NodeType) > 0 {
		slog.Info("只筛选用户设置的协议", "type", config.GlobalConfig.NodeType)
	}

	var wg sync.WaitGroup
	// Subscription-fetch concurrency is decoupled from the alive-check
	// concurrency: most users want dozens of cheap HTTP fetches in parallel
	// even when the alive-check pool is tuned large. Falls back to 20 if
	// the user hasn't set it (zero value).
	subFetchConcurrency := config.GlobalConfig.SubUrlsConcurrent
	if subFetchConcurrency <= 0 {
		subFetchConcurrency = 20
	}
	concurrentLimit := make(chan struct{}, subFetchConcurrency) // 限制并发数

	// 按订阅顺序预分配槽位,每个 goroutine 只写自己的下标,无竞争
	// 这样即便是并发获取,最终合并时仍能保持 subUrls 的顺序(本地在前,远程在后)
	buckets := make([][]map[string]any, len(subUrls))

	// 启动工作协程
	for idx, subUrl := range subUrls {
		wg.Add(1)
		concurrentLimit <- struct{}{} // 获取令牌

		go func(i int, e subEntry) {
			defer wg.Done()
			defer func() { <-concurrentLimit }() // 释放令牌

			url := e.url
			data, userinfo, err := GetDateFromSubs(url)
			if err != nil {
				slog.Error("获取订阅链接错误跳过", "source", e.source, "url", url, "err", err)
				return
			}
			if userinfo != "" {
				subUserinfoMap.Store(url, userinfo)
			}

			var tag string
			if d, err := u.Parse(url); err == nil {
				tag = d.Fragment
			}

			var local []map[string]any

			var con map[string]any
			err = yaml.Unmarshal(data, &con)
			if err != nil {
				// Some subscription providers return base64-encoded Clash YAML
				// config when they detect a Clash-like User-Agent. The YAML may
				// also contain duplicate top-level keys (providers concatenate
				// multiple config snippets). Try to decode and extract proxies.
				con = tryDecodeBase64ClashYAML(data)
			}
			if con == nil {
				data = fixVMessURLSafeBase64(data)
				proxyList, err := convert.ConvertsV2Ray(data)
				if err != nil {
					slog.Error("解析proxy错误", "source", e.source, "url", url, "err", err)
					return
				}
				slog.Debug("获取订阅链接", "source", e.source, "url", url, "count", len(proxyList))
				local = make([]map[string]any, 0, len(proxyList))
				for _, proxy := range proxyList {
					// 只测试指定协议
					if t, ok := proxy["type"].(string); ok {
						if len(config.GlobalConfig.NodeType) > 0 && !lo.Contains(config.GlobalConfig.NodeType, t) {
							continue
						}
					}

					// 为每个节点添加订阅链接来源信息和备注
					proxy["sub_url"] = url
					if tag != "" {
						proxy["sub_tag"] = tag
					}
					local = append(local, proxy)
				}
				buckets[i] = local
				return
			}

			proxyInterface, ok := con["proxies"]
			if !ok || proxyInterface == nil {
				slog.Error("订阅链接没有proxies", "source", e.source, "url", url)
				return
			}

			proxyList, ok := proxyInterface.([]any)
			if !ok {
				return
			}
			slog.Debug("获取订阅链接", "source", e.source, "url", url, "count", len(proxyList))
			local = make([]map[string]any, 0, len(proxyList))
			for _, proxy := range proxyList {
				if proxyMap, ok := proxy.(map[string]any); ok {
					if t, ok := proxyMap["type"].(string); ok {
						// 只测试指定协议
						if len(config.GlobalConfig.NodeType) > 0 && !lo.Contains(config.GlobalConfig.NodeType, t) {
							continue
						}
						// 虽然支持mihomo支持下划线，但是这里为了规范，还是改成横杠
						// todo: 不知道后边还有没有这类问题
						switch t {
						case "hysteria2", "hy2":
							if _, ok := proxyMap["obfs_password"]; ok {
								proxyMap["obfs-password"] = proxyMap["obfs_password"]
								delete(proxyMap, "obfs_password")
							}
						}
					}
					// 为每个节点添加订阅链接来源信息和备注
					proxyMap["sub_url"] = url
					if tag != "" {
						proxyMap["sub_tag"] = tag
					}
					local = append(local, proxyMap)
				}
			}
			buckets[i] = local
		}(idx, subEntry{url: utils.WarpUrl(subUrl.url), source: subUrl.source})
	}

	// 等待所有工作协程完成
	wg.Wait()

	// 按订阅顺序合并,保证本地订阅在前、远程订阅在后,订阅内节点顺序也保留
	total := 0
	for _, b := range buckets {
		total += len(b)
	}
	mihomoProxies := make([]map[string]any, 0, total)
	for _, b := range buckets {
		mihomoProxies = append(mihomoProxies, b...)
	}

	return mihomoProxies, nil
}

// from 3k
// resolveSubUrls 合并本地与远程订阅清单并去重
func resolveSubUrls() ([]subEntry, int, int) {
	// 计数
	var localNum, remoteNum int
	localNum = len(config.GlobalConfig.SubUrls)

	entries := make([]subEntry, 0, len(config.GlobalConfig.SubUrls))
	// 本地配置
	for _, u := range config.GlobalConfig.SubUrls {
		entries = append(entries, subEntry{url: u, source: "本地配置"})
	}

	// 远程清单
	if len(config.GlobalConfig.SubUrlsRemote) != 0 {
		for _, d := range config.GlobalConfig.SubUrlsRemote {
			if remote, err := fetchRemoteSubUrls(utils.WarpUrl(d)); err != nil {
				slog.Warn("获取远程订阅清单失败，已忽略", "url", d, "err", err)
			} else {
				remoteNum += len(remote)
				for _, u := range remote {
					entries = append(entries, subEntry{url: u, source: d})
				}
			}
		}
	}

	// 规范化与去重
	seen := make(map[string]struct{}, len(entries))
	out := make([]subEntry, 0, len(entries))
	for _, e := range entries {
		s := strings.TrimSpace(e.url)
		if s == "" || strings.HasPrefix(s, "#") { // 跳过空行与注释
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, subEntry{url: s, source: e.source})
	}
	return out, localNum, remoteNum
}

// fetchRemoteSubUrls 从远程地址读取订阅URL清单
// 支持两种格式：
// 1) 纯文本，按换行分隔，支持以 # 开头的注释与空行
// 2) YAML/JSON 的字符串数组
func fetchRemoteSubUrls(listURL string) ([]string, error) {
	if listURL == "" {
		return nil, errors.New("empty list url")
	}
	data, _, err := GetDateFromSubs(listURL)
	if err != nil {
		return nil, err
	}

	// 优先尝试解析为字符串数组（YAML/JSON兼容）
	var arr []string
	if err := yaml.Unmarshal(data, &arr); err == nil && len(arr) > 0 {
		return arr, nil
	}

	// 回退为按行解析
	res := make([]string, 0, 16)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		res = append(res, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// 订阅链接中获取数据,同时返回 Subscription-Userinfo 响应头
func GetDateFromSubs(subUrl string) ([]byte, string, error) {
	maxRetries := config.GlobalConfig.SubUrlsReTry
	// 重试间隔
	retryInterval := config.GlobalConfig.SubUrlsRetryInterval
	if retryInterval == 0 {
		retryInterval = 1
	}
	// 超时时间
	timeout := config.GlobalConfig.SubUrlsTimeout
	if timeout == 0 {
		timeout = 10
	}
	var lastErr error

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	// Route DNS through the configured mihomo resolver so subscription domains aren't leaked to system DNS.
	// Only when user enabled custom DNS — keeps default behavior unchanged for existing users.
	if config.GlobalConfig.DNS.Enable {
		transport.DialContext = newMihomoDialer(time.Duration(timeout) * time.Second)
	}
	client := &http.Client{
		Timeout:   time.Duration(timeout) * time.Second,
		Transport: transport,
	}
	// 函数返回时关闭空闲连接，避免 transport 的 idle conn 积压导致内存不释放
	defer client.CloseIdleConnections()

	for i := range maxRetries {
		if i > 0 {
			time.Sleep(time.Duration(retryInterval) * time.Second)
		}

		req, err := http.NewRequest("GET", subUrl, nil)
		if err != nil {
			lastErr = err
			continue
		}

		if config.GlobalConfig.SubUrlsGetUA == "random" {
			req.Header.Set("User-Agent", convert.RandUserAgent())
		} else {
			req.Header.Set("User-Agent", config.GlobalConfig.SubUrlsGetUA)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			lastErr = fmt.Errorf("返回状态码: %d", resp.StatusCode)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		userinfo := resp.Header.Get("Subscription-Userinfo")
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("读取响应数据错误: %w", err)
			continue
		}
		return body, userinfo, nil
	}

	return nil, "", fmt.Errorf("重试%d次后失败: %w", maxRetries, lastErr)
}

// newMihomoDialer returns a DialContext that resolves via mihomo's global resolver
// (DoH when configured) then dials the resulting IP directly, avoiding the OS DNS path.
// dialTimeout is shared with the request-level timeout from SubUrlsTimeout.
func newMihomoDialer(dialTimeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: dialTimeout}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		// If addr is already an IP, skip resolution.
		if ip := net.ParseIP(host); ip != nil {
			return d.DialContext(ctx, network, addr)
		}
		ips, err := resolver.LookupIP(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no ip for %s", host)
		}
		// Try each IP in turn, returning on the first successful connection.
		var dialErr error
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			dialErr = err
		}
		return nil, fmt.Errorf("dial %s: %w", host, dialErr)
	}
}

// fixVMessURLSafeBase64 preprocesses vmess share links to fix two issues that
// cause mihomo's tryDecodeBase64 to fail, resulting in vmess nodes being
// silently skipped with "url.Port() is empty" warnings:
//  1. URL-safe base64 characters (- and _) in the vmess body — mihomo only
//     tries RawStdEncoding and StdEncoding, not URLEncoding.
//  2. A "#fragment" suffix appended after the base64 body — the "#" character
//     is not valid base64 and causes CorruptInputError. The fragment is
//     redundant because the node name is already in the JSON "ps" field.
func fixVMessURLSafeBase64(data []byte) []byte {
	// Try to decode outer base64 (subscription data is typically base64-encoded)
	var decoded []byte
	var wasBase64 bool
	if d, err := base64.StdEncoding.DecodeString(string(data)); err == nil {
		decoded = d
		wasBase64 = true
	} else if d, err := base64.RawStdEncoding.DecodeString(string(data)); err == nil {
		decoded = d
		wasBase64 = true
	} else {
		// Not base64 - treat as plain text proxy URLs
		decoded = data
	}

	// Fix vmess links
	lines := strings.Split(string(decoded), "\n")
	modified := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "vmess://") {
			continue
		}

		original := trimmed

		// Strip "#fragment" suffix — "#" is not valid base64 and causes
		// tryDecodeBase64 to fail. The node name is in the JSON "ps" field.
		if idx := strings.Index(trimmed, "#"); idx >= 0 {
			trimmed = trimmed[:idx]
		}

		// Convert URL-safe base64 characters to standard base64
		body := strings.TrimPrefix(trimmed, "vmess://")
		if strings.ContainsAny(body, "-_") {
			body = strings.ReplaceAll(body, "-", "+")
			body = strings.ReplaceAll(body, "_", "/")
		}

		lines[i] = "vmess://" + body
		if lines[i] != original {
			modified = true
		}
	}

	if !modified {
		return data
	}

	fixed := []byte(strings.Join(lines, "\n"))
	// If original was base64-encoded, re-encode to keep ConvertsV2Ray's
	// DecodeBase64 path working the same way
	if wasBase64 {
		return []byte(base64.StdEncoding.EncodeToString(fixed))
	}
	return fixed
}

// tryDecodeBase64ClashYAML attempts to decode base64-encoded Clash YAML config
// data and extract proxy entries. This handles subscription providers that
// return different content based on User-Agent: when a Clash-like UA is used,
// they may return base64-encoded Clash YAML instead of V2Ray share links.
//
// The YAML may contain duplicate top-level "proxies:" keys (providers
// concatenate multiple config snippets). yaml.v3 rejects duplicate keys, so
// when that happens we fall back to extracting flow-style proxy entries
// ("- {name: ..., type: ...}") line by line.
//
// Returns a map with a "proxies" key containing the extracted proxy list,
// or nil if the data is not base64-encoded Clash YAML.
func tryDecodeBase64ClashYAML(data []byte) map[string]any {
	// Step 1: Try base64 decode
	var decoded []byte
	if d, err := base64.StdEncoding.DecodeString(string(data)); err == nil {
		decoded = d
	} else if d, err := base64.RawStdEncoding.DecodeString(string(data)); err == nil {
		decoded = d
	} else {
		return nil
	}

	// Step 2: Try standard YAML parse (handles clean configs without duplicates)
	var con map[string]any
	if err := yaml.Unmarshal(decoded, &con); err == nil {
		if _, ok := con["proxies"]; ok {
			return con
		}
		return nil
	}

	// Step 3: YAML has duplicate keys — extract flow-style proxy entries
	// from text. Each entry is a single-line YAML flow map like:
	//   - {name: ..., server: ..., type: vless, ...}
	lines := strings.Split(string(decoded), "\n")
	inProxies := false
	var proxyList []any
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		// Track top-level "proxies:" sections (no indentation)
		if line == "proxies:" {
			inProxies = true
			continue
		}
		// Exit proxies mode on any other top-level key
		if len(line) > 0 && line[0] != ' ' && line[0] != '-' && line[0] != '\t' {
			inProxies = false
			continue
		}
		// Parse flow-style entries ("- {name: ...}")
		if inProxies && strings.HasPrefix(line, "- {") {
			entry := strings.TrimPrefix(line, "- ")
			var p map[string]any
			if err := yaml.Unmarshal([]byte(entry), &p); err == nil {
				proxyList = append(proxyList, p)
			}
		}
	}

	if len(proxyList) == 0 {
		return nil
	}
	return map[string]any{"proxies": proxyList}
}
