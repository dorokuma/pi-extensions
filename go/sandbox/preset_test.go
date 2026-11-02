package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeUsageJSON 写 <dir>/usage.json 为指定 lastUsed（Unix 秒）。
func writeUsageJSON(t *testing.T, dir string, lastUsed int64) {
	t.Helper()
	data, err := json.Marshal(struct {
		LastUsed int64 `json:"lastUsed"`
	}{LastUsed: lastUsed})
	if err != nil {
		t.Fatalf("marshal usage.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), data, 0o644); err != nil {
		t.Fatalf("write usage.json: %v", err)
	}
}

// readUsageLastUsed 读回 <dir>/usage.json 的 lastUsed。
func readUsageLastUsed(t *testing.T, dir string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatalf("read usage.json: %v", err)
	}
	var usage struct {
		LastUsed int64 `json:"lastUsed"`
	}
	if err := json.Unmarshal(data, &usage); err != nil {
		t.Fatalf("unmarshal usage.json: %v", err)
	}
	return usage.LastUsed
}

// TestTouchUsageJSONSkipWindow 覆盖 touchUsageJSON 的 <5m 跳过重写窗口：
// <5m 只读短路不改写、>=5m（含恰 5m 边界）原子更新 lastUsed、未来时间戳短路不改写。
func TestTouchUsageJSONSkipWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0) // 固定基准，避免与真实时钟漂移

	cases := []struct {
		name         string
		lastUsed     int64
		wantRewrites bool
	}{
		{"fresh_4m_skip", now.Add(-4 * time.Minute).Unix(), false},
		{"boundary_5m_rewrite", now.Add(-5 * time.Minute).Unix(), true},
		{"stale_6m_rewrite", now.Add(-6 * time.Minute).Unix(), true},
		{"future_1h_skip", now.Add(time.Hour).Unix(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeUsageJSON(t, dir, tc.lastUsed)
			if err := touchUsageJSON(dir, now); err != nil {
				t.Fatalf("touchUsageJSON: %v", err)
			}
			got := readUsageLastUsed(t, dir)
			if tc.wantRewrites {
				if got != now.Unix() {
					t.Fatalf("expected lastUsed rewritten to %d, got %d", now.Unix(), got)
				}
			} else if got != tc.lastUsed {
				t.Fatalf("expected lastUsed unchanged %d, got %d", tc.lastUsed, got)
			}
		})
	}
}
