package proxies

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jadylc/subs-check/config"
)

func TestNewSubsTransport_ProxyToggle(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")

	withProxy := newSubsTransport(true, 5*time.Second)
	if withProxy.Proxy == nil {
		t.Fatal("expected Proxy set when useProxy=true")
	}

	noProxy := newSubsTransport(false, 5*time.Second)
	if noProxy.Proxy != nil {
		t.Fatal("expected Proxy nil when useProxy=false")
	}
}

// TestGetDateFromSubs_RetryFallsBackToDirect 验证: 首次尝试走不可用代理失败后,
// 重试能切到直连并成功获取数据。
func TestGetDateFromSubs_RetryFallsBackToDirect(t *testing.T) {
	// 环境无 NO_PROXY 放行, 代理指向必然拒绝连接的地址
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok-body"))
	}))
	defer ts.Close()

	oldRetry := config.GlobalConfig.SubUrlsReTry
	oldInterval := config.GlobalConfig.SubUrlsRetryInterval
	oldTimeout := config.GlobalConfig.SubUrlsTimeout
	oldUA := config.GlobalConfig.SubUrlsGetUA
	config.GlobalConfig.SubUrlsReTry = 2
	config.GlobalConfig.SubUrlsRetryInterval = 0
	config.GlobalConfig.SubUrlsTimeout = 5
	config.GlobalConfig.SubUrlsGetUA = "test-ua"
	defer func() {
		config.GlobalConfig.SubUrlsReTry = oldRetry
		config.GlobalConfig.SubUrlsRetryInterval = oldInterval
		config.GlobalConfig.SubUrlsTimeout = oldTimeout
		config.GlobalConfig.SubUrlsGetUA = oldUA
	}()

	data, userinfo, err := GetDateFromSubs(ts.URL)
	if err != nil {
		t.Fatalf("expected direct retry to succeed, got error: %v", err)
	}
	if string(data) != "ok-body" {
		t.Errorf("unexpected body: %q", string(data))
	}
	if userinfo != "" {
		t.Errorf("unexpected userinfo: %q", userinfo)
	}
}
