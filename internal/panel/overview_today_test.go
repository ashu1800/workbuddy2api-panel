package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// TestMergeTodayUsage 账号表的「今日调用/今日用量/成功率」由用量记录器的今天窗口合并而来：
// 同名 uid 取今日数，今日无流量的账号补零（0 是真实值，不是"没有数据"）。
func TestMergeTodayUsage(t *testing.T) {
	accts := []pool.Status{
		{UID: "u1", Nickname: "有流量"},
		{UID: "u2", Nickname: "今天没跑"},
	}
	byUID := map[string]usage.KeyedAgg{
		"u1": {Key: "u1", Agg: usage.Agg{Requests: 42, Errors: 3, TotalTokens: 12345}},
	}
	out := mergeTodayUsage(accts, byUID, "2026-09-30")
	if len(out) != 2 {
		t.Fatalf("merged %d accounts, want 2", len(out))
	}
	if out[0].Today.Requests != 42 || out[0].Today.Errors != 3 || out[0].Today.Tokens != 12345 {
		t.Errorf("u1 today=%+v, want 42/3/12345", out[0].Today)
	}
	if out[0].Today.Day != "2026-09-30" {
		t.Errorf("day=%q, want 2026-09-30", out[0].Today.Day)
	}
	if out[1].Today.Requests != 0 || out[1].Today.Errors != 0 || out[1].Today.Tokens != 0 {
		t.Errorf("u2 today=%+v, want zeros", out[1].Today)
	}
	// 嵌入的账号状态必须仍然平铺在顶层（面板按 s.uid/s.credits 直接取用）
	if out[0].UID != "u1" || out[0].Nickname != "有流量" {
		t.Errorf("status fields lost in merge: %+v", out[0].Status)
	}
}

// TestOverviewTodayUsage 端到端：/panel/api/overview 必须带上今日自然日聚合，
// 且 today 与池里的累计口径分开（累计仍在 token_usage 里，窗口不同不能混用）。
func TestOverviewTodayUsage(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "acct"})
	// 池侧累计：3 次尝试（2 成 1 败）+ 1000 token
	p.RecordTokenUsage("u1", pool.TokenUsageDelta{OK: true, HasTotalTokens: true, TotalTokens: 600})
	p.RecordTokenUsage("u1", pool.TokenUsageDelta{OK: true, HasTotalTokens: true, TotalTokens: 400})
	p.RecordTokenUsage("u1", pool.TokenUsageDelta{})

	rec := usage.New("")
	now := time.Now()
	rec.Add(now, "cn", "u1", "m", usage.Delta{TotalTokens: 600, HasTotal: true}, true)
	rec.Add(now, "cn", "u1", "m", usage.Delta{TotalTokens: 400, HasTotal: true}, true)
	rec.Add(now, "cn", "u1", "m", usage.Delta{}, false)

	panel := New(Config{Version: "test", Pool: p, Usage: rec})
	w := httptest.NewRecorder()
	panel.ServeHTTP(w, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("overview status=%d", w.Code)
	}
	var got struct {
		Accounts []struct {
			UID   string `json:"uid"`
			Today struct {
				Day      string `json:"day"`
				Requests int64  `json:"requests"`
				Errors   int64  `json:"errors"`
				Tokens   int64  `json:"total_tokens"`
			} `json:"today"`
			TokenUsage struct {
				RequestCount int64 `json:"request_count"`
				OKCount      int64 `json:"ok_count"`
				TotalTokens  int64 `json:"total_tokens"`
			} `json:"token_usage"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts=%d, want 1", len(got.Accounts))
	}
	a := got.Accounts[0]
	if a.Today.Day != now.Format("2006-01-02") {
		t.Errorf("today.day=%q, want %q", a.Today.Day, now.Format("2006-01-02"))
	}
	// 今天窗口从 00:00 起算，故刚写入的 3 次尝试必然全部在窗口内
	if a.Today.Requests != 3 || a.Today.Errors != 1 || a.Today.Tokens != 1000 {
		t.Errorf("today=%+v, want 3/1/1000", a.Today)
	}
	// 累计口径必须仍然独立存在（悬浮提示对照用）
	if a.TokenUsage.RequestCount != 3 || a.TokenUsage.OKCount != 2 || a.TokenUsage.TotalTokens != 1000 {
		t.Errorf("cumulative token_usage=%+v, want 3/2/1000", a.TokenUsage)
	}
}
