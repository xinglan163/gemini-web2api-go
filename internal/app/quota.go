package app

// 网页端「用量额度」（Usage limits）——GET /admin/api/quota。
//
// rpc id 与响应结构来自对 gemini.google.com/usage 的逆向，并已用真实账号核对：
//   页面显示 5 小时窗口 63% / 每周 13%  ⇄  本接口返回同样的 63% / 13% 与同样的重置时间。
// 参考实现（MIT）：https://github.com/Nagi-ovo/voyager —— public/usage-observer.js
// 与 src/pages/content/usageStatus/usageParsing.ts（extractUsagePayload / parseUsageRpcResponse）。
//
// 协议：
//   POST https://gemini.google.com/_/BardChatUi/data/batchexecute
//        ?rpcids=jSf9Qc&bl=<bl>&hl=en&_reqid=<n>&rt=c&source-path=/usage
//   f.req=[[["jSf9Qc","[]",null,"generic"]]]  &at=<XSRF>
// 响应是标准 batchexecute 分帧，载荷里有一个数组，元素形如
//   [limit, fraction, period, [[resetEpoch, nanos]]]
// 其中 fraction 是「已用比例」(0..1)、period 1 = 5 小时滚动窗口、2 = 每周额度。
//
// 额度是**账号级**的（匿名没有），所以池里必须有可用 cookie。
//
// 额度是慢变量，但每次查询都要真实出网一次（占一个 cookie + 一条出口），
// 所以结果缓存 20 秒；**失败不写缓存**，下一次请求立刻重试。

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// QuotaMetric 是一档额度。
type QuotaMetric struct {
	UsedPercent int   `json:"used_percent"` // 已用百分比（0-150）
	RemainPct   int   `json:"remain_percent"`
	ResetAt     int64 `json:"reset_at"`     // unix 秒
	ResetInSec  int64 `json:"reset_in_sec"` // 距重置还有多少秒
}

// QuotaSnapshot 是 /admin/api/quota 的返回体。
type QuotaSnapshot struct {
	OK        bool         `json:"ok"`
	AccountID int64        `json:"account_id,omitempty"`
	Account   string       `json:"account,omitempty"`
	Proxy     string       `json:"proxy,omitempty"`
	Direct    bool         `json:"direct"`
	FiveHour  *QuotaMetric `json:"five_hour,omitempty"`
	Weekly    *QuotaMetric `json:"weekly,omitempty"`
	FetchedAt int64        `json:"fetched_at"`
	Cached    bool         `json:"cached,omitempty"`
	Error     string       `json:"error,omitempty"`
}

var (
	quotaMu     sync.Mutex
	quotaCache  *QuotaSnapshot
	quotaCacheT time.Time
)

// quotaCacheTTL 让插件即便每分钟轮询也不会把上游打爆。网页端自己显示「Updated just now」，
// 但额度是慢变量，20 秒足够新。
const quotaCacheTTL = 20 * time.Second

// quotaAccount 选出最该用的账号，**不改写 last_used_at**（额度查询不该推进轮转）。
func quotaAccount() (*CookieAccount, bool) {
	accts := accountList()
	cands := make([]CookieAccount, 0, len(accts))
	for _, a := range accts {
		if a.Status == "enabled" {
			cands = append(cands, a)
		}
	}
	if len(cands) == 0 {
		return nil, false
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].FailCount != cands[j].FailCount {
			return cands[i].FailCount < cands[j].FailCount
		}
		if cands[i].LastUsedAt != cands[j].LastUsedAt {
			return cands[i].LastUsedAt < cands[j].LastUsedAt
		}
		return cands[i].ID < cands[j].ID
	})
	return &cands[0], true
}

// quotaProxy 挑出口：优先账号绑定的那个，否则代理池里第一个健康的，池空则直连。
// 与 handleAdminTest 同款（旁路查询，不占限流 slot）。
func quotaProxy(preferID int64) (proxyURL, proxyName string, direct bool) {
	for _, p := range listProxies() {
		if !p.Enabled || p.FailCount >= proxyFailThreshold {
			continue
		}
		if preferID != 0 && p.ID != preferID {
			continue
		}
		return p.URL, p.Name, false
	}
	if preferID != 0 {
		for _, p := range listProxies() {
			if p.Enabled && p.FailCount < proxyFailThreshold {
				return p.URL, p.Name, false
			}
		}
	}
	return "", "直连", true
}

// fetchQuotaRPC 真打一次上游拿额度。
func fetchQuotaRPC() QuotaSnapshot {
	snap := QuotaSnapshot{FetchedAt: time.Now().Unix()}

	acct, ok := quotaAccount()
	if !ok {
		snap.Error = "cookie 池里没有启用的账号：额度是账号级的，先挂 Google cookie（面板 Cookie 池）"
		return snap
	}
	snap.AccountID, snap.Account = acct.ID, acct.Label
	proxyURL, proxyName, direct := quotaProxy(acct.ProxyID)
	snap.Proxy, snap.Direct = proxyName, direct

	xsrf, err := getXSRF(acct.Cookie, proxyURL)
	if err != nil || xsrf == "" {
		snap.Error = "取 XSRF token 失败（cookie 可能已失效，面板点「检测」确认）: " + errText(err)
		return snap
	}

	// f.req 逐字对照 Voyager 抓包：args 是空数组的字符串 "[]"，不是 ""。
	form := url.Values{}
	form.Set("f.req", `[[["jSf9Qc","[]",null,"generic"]]]`)
	form.Set("at", xsrf)

	reqid := time.Now().UnixNano() % 1000000
	endpoint := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=jSf9Qc&bl=%s&hl=en&_reqid=%d&rt=c&source-path=%%2Fusage",
		currentBL(proxyURL), reqid)

	headers := buildGeminiHeaders(acct.Cookie, extractSAPISID(acct.Cookie), "")
	delete(headers, "x-goog-ext-525001261-jspb") // batchexecute 不带模型 header

	status, _, body, err := uploadPost(endpoint, headers, []byte(form.Encode()), proxyURL)
	if err != nil {
		snap.Error = "网络错误: " + err.Error()
		return snap
	}
	if status != 200 {
		snap.Error = fmt.Sprintf("上游 HTTP %d（登录态可能已过期）", status)
		return snap
	}

	five, weekly, perr := parseQuotaBody(string(body))
	if perr != nil {
		snap.Error = perr.Error()
		return snap
	}
	now := time.Now().Unix()
	hydrate := func(m *QuotaMetric) *QuotaMetric {
		if m == nil {
			return nil
		}
		if m.ResetAt > now {
			m.ResetInSec = m.ResetAt - now
		}
		m.RemainPct = 100 - m.UsedPercent
		if m.RemainPct < 0 {
			m.RemainPct = 0
		}
		return m
	}
	snap.FiveHour, snap.Weekly = hydrate(five), hydrate(weekly)
	if snap.FiveHour == nil && snap.Weekly == nil {
		snap.Error = "响应里没有可识别的额度档位（上游可能改版）"
		return snap
	}
	snap.OK = true
	return snap
}

// getQuota 带缓存地取额度。
func getQuota(force bool) QuotaSnapshot {
	quotaMu.Lock()
	defer quotaMu.Unlock()
	if !force && quotaCache != nil && time.Since(quotaCacheT) < quotaCacheTTL {
		c := *quotaCache
		c.Cached = true
		return c
	}
	snap := fetchQuotaRPC()
	if snap.OK { // 失败不写缓存，下次立刻重试
		quotaCache = &snap
		quotaCacheT = time.Now()
	}
	return snap
}

// handleAdminQuota — GET /admin/api/quota[?refresh=1]
//
//	{"ok":true,"account":"Ultra(FF导入)","five_hour":{"used_percent":63,...},"weekly":{...}}
func handleAdminQuota(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		force := r.URL.Query().Get("refresh") == "1"
		writeJSON(w, 200, getQuota(force))
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// parseQuotaBody 从 batchexecute 原始响应里抽出 5 小时 / 每周两档额度。
//
// 沿用 Voyager 的判据（extractUsagePayload）：不假设固定下标，而是遍历整个载荷，
// 找出「元素形如 [limit, fraction, period, [[epoch, nanos]]]」的那一组——
// 上游将来加新的额度桶也不会整段解析失败。
func parseQuotaBody(raw string) (five, weekly *QuotaMetric, err error) {
	for _, payload := range quotaPayloads(raw) {
		arr, ok := payload.([]interface{})
		if !ok {
			continue
		}
		var metrics []*QuotaMetric
		var periods []float64
		for _, bucket := range arr {
			items, ok := bucket.([]interface{})
			if !ok {
				continue
			}
			var got []*QuotaMetric
			var gotP []float64
			for _, it := range items {
				m, p, ok := readQuotaMetric(it)
				if !ok {
					continue
				}
				got = append(got, m)
				gotP = append(gotP, p)
			}
			if len(got) > 0 {
				metrics, periods = got, gotP
				break
			}
		}
		if len(metrics) == 0 {
			continue
		}
		for i, p := range periods {
			switch int(p) {
			case 1: // 5 小时滚动窗口
				if five == nil {
					five = metrics[i]
				}
			case 2: // 每周
				if weekly == nil {
					weekly = metrics[i]
				}
			}
		}
		if five != nil || weekly != nil {
			return five, weekly, nil
		}
	}
	return nil, nil, fmt.Errorf("额度字段解析失败：响应结构可能与预期不符（上游改版）")
}

// quotaPayloads 取出 batchexecute 分帧里每个 wrb.fr 帧的载荷（inner JSON 解出来的对象）。
func quotaPayloads(raw string) []interface{} {
	var out []interface{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var frame []interface{}
		if json.Unmarshal([]byte(line), &frame) != nil {
			continue
		}
		for _, part := range frame {
			parts, ok := part.([]interface{})
			if !ok || len(parts) < 3 || parts[0] != "wrb.fr" {
				continue
			}
			s, ok := parts[2].(string)
			if !ok {
				continue
			}
			var payload interface{}
			if json.Unmarshal([]byte(s), &payload) == nil {
				out = append(out, payload)
			}
		}
	}
	return out
}

// readQuotaMetric 判据同 Voyager 的 readMetric：
// [limit, fraction(0..1.5), period, [[resetEpoch, nanos]]]。
func readQuotaMetric(v interface{}) (*QuotaMetric, float64, bool) {
	m, ok := v.([]interface{})
	if !ok || len(m) < 4 {
		return nil, 0, false
	}
	frac, ok := m[1].(float64)
	if !ok || frac < 0 || frac > 1.5 {
		return nil, 0, false
	}
	period, ok := m[2].(float64)
	if !ok {
		return nil, 0, false
	}
	rw, ok := m[3].([]interface{})
	if !ok || len(rw) == 0 {
		return nil, 0, false
	}
	head, ok := rw[0].([]interface{})
	if !ok || len(head) == 0 {
		return nil, 0, false
	}
	epoch, ok := head[0].(float64)
	if !ok || epoch < 1_600_000_000 {
		return nil, 0, false
	}
	return &QuotaMetric{UsedPercent: int(math.Round(frac * 100)), ResetAt: int64(epoch)}, period, true
}

// errText 把可能为 nil 的 error 变成可读文本。
func errText(err error) string {
	if err == nil {
		return "未知原因"
	}
	return err.Error()
}
