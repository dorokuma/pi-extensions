package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// 只读角色回报中继（沙盒外 sbx 外层父进程内运行）：
// 仅 --preset reviewer-agent 且 --report-to 有效且可信 hc 存在时启用。
// 在 <goExecDir>/report.sock（0600）监听，先于 cmd.Start() 就绪；沙盒对
// goExecDir 只读，删不掉也换不掉该 socket。每连接设独立读写 deadline；
// 帧格式为 4 字节大端长度前缀 + JSON {"to","text"}，整包上限 64KiB。
// 校验顺序：to 必须等于父地址 → UTF-8 规范化并剔除除 \n\t 外控制字符 →
// 超 8000 字节在字符边界截断 → 令牌桶（容量 3，每 30 秒补 1，每会话 20 条）。
// 执行 exec.CommandContext(hcPath,"send",parent,text)：不经 shell，超时 5s，
// 文本以 "-" 开头时前置空格（hc send 的 clap 参数解析不接受 "-" 开头的
// TEXT 操作数；hc 0.1.14 提示可用 "-- -x"，但按设计不依赖该特性）。
// 回包 1 字节状态（0 成功 / 1 拒绝 / 2 投递失败）+ 简短原因。

const (
	reportSockName            = "report.sock"
	reportMaxFrame            = 64 << 10 // 整包上限 64 KiB
	reportMaxTextBytes        = 8000
	reportTruncSuffix         = "…[truncated by sbx]"
	reportBurstTokens         = 3
	reportRefillEvery         = 30 * time.Second
	reportSessionMax          = 20
	reportHcTimeout           = 5 * time.Second
	reportDrain               = 6 * time.Second // 大于 hc 超时，保证在途投递可收尾
	reportReadTimeout         = 5 * time.Second
	reportMaxReasonLen        = 120
	reportStatusOK       byte = 0
	reportStatusRejected byte = 1
	reportStatusFailed   byte = 2
)

// reportConnReadTimeout 为每连接读 deadline，单测可调小以覆盖慢连接超时。
var reportConnReadTimeout = reportReadTimeout

// hcPathOverride 仅用于测试构建以 -ldflags -X 注入假 hc 路径；
// 生产构建不注入时为空，hc 只在固定可信位置解析，绝不走 PATH 或环境变量。
var hcPathOverride = ""

var reportAddrRe = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,64}$`)

// reportRequest 是沙盒内 hc 客户端发送到 report.sock 的请求体。
type reportRequest struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

// reportRelay 保存中继运行态：监听器、令牌桶与在途请求 WaitGroup。
type reportRelay struct {
	parent  string
	hcPath  string
	ln      net.Listener
	wg      sync.WaitGroup
	mu      sync.Mutex
	tokens  int
	sent    int
	lastRef time.Time
}

// maybeStartReportRelay 在 cmd.Start() 之前启动回报中继；未满足启用条件或
// 监听失败时返回 nil（沙盒照常启动，仅回报能力关闭，fail-open 为可观察降级，
// 客户端侧以退出码 75 报告中继不可用）。
func maybeStartReportRelay(cfg cliConfig, policyEnv []string) *reportRelay {
	if cfg.Preset != "reviewer-agent" || strings.TrimSpace(cfg.ReportTo) == "" {
		return nil
	}
	parent := cfg.ReportTo
	if !reportAddrRe.MatchString(parent) {
		fmt.Fprintf(os.Stderr, "sbx: report relay disabled: invalid --report-to %q\n", parent)
		return nil
	}
	// 外层 HC_TERMINAL_ID 存在且等于 --report-to 时拒绝启用（防止发给自己）；
	// 外层缺该变量时跳过这项校验。
	if self := os.Getenv("HC_TERMINAL_ID"); self != "" && self == parent {
		fmt.Fprintf(os.Stderr, "sbx: report relay disabled: --report-to %q equals own HC_TERMINAL_ID\n", parent)
		return nil
	}
	hcPath := locateTrustedHc()
	if hcPath == "" {
		fmt.Fprintf(os.Stderr, "sbx: report relay disabled: hc not found in trusted locations\n")
		return nil
	}
	goExecDir := envListValue(policyEnv, "PI_SANDBOX_GO_EXEC_DIR")
	if goExecDir == "" {
		return nil
	}
	sockPath := filepath.Join(goExecDir, reportSockName)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx: report relay disabled: listen %s: %v\n", sockPath, err)
		return nil
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "sbx: report relay disabled: chmod %s: %v\n", sockPath, err)
		_ = ln.Close()
		return nil
	}
	r := &reportRelay{
		parent:  parent,
		hcPath:  hcPath,
		ln:      ln,
		tokens:  reportBurstTokens,
		lastRef: time.Now(),
	}
	go r.serve()
	return r
}

// locateTrustedHc 只在固定可信位置查找 hc：/root/.local/bin/hc 与
// trustedHomeDir 同路径（euid 对应 home 的 .local/bin/hc）。绝不使用调用方 PATH。
func locateTrustedHc() string {
	if hcPathOverride != "" {
		return hcPathOverride
	}
	for _, cand := range []string{
		"/root/.local/bin/hc",
		filepath.Join(mainTrustedHomeDir(), ".local", "bin", "hc"),
	} {
		if st, err := os.Stat(cand); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return cand
		}
	}
	return ""
}

// mainTrustedHomeDir 与 go/sandbox/warm.go 的 trustedHomeDir 同口径：
// euid 经 os/user.LookupId 查 /etc/passwd，euid==0 兜底 /root；
// 绝不读 os.Getenv("HOME")。
func mainTrustedHomeDir() string {
	if u, err := user.LookupId(strconv.Itoa(os.Geteuid())); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	if os.Geteuid() == 0 {
		return "/root"
	}
	return ""
}

func (r *reportRelay) serve() {
	for {
		c, err := r.ln.Accept()
		if err != nil {
			return // 监听已关闭
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.handleConn(c)
		}()
	}
}

// close 先关监听器（accept 循环退出），再最多等 reportDrain 秒收尾在途请求。
func (r *reportRelay) close() {
	if r == nil {
		return
	}
	_ = r.ln.Close()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(reportDrain):
	}
}

// handleConn 处理一个沙盒内 hc 客户端连接：读 deadline 防慢连接挂死，
// 4 字节长度前缀限帧 64KiB，回包 1 字节状态 + 原因。
func (r *reportRelay) handleConn(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(reportConnReadTimeout))
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		r.reply(c, reportStatusRejected, "bad frame")
		return
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > reportMaxFrame {
		r.reply(c, reportStatusRejected, "frame too large")
		return
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		r.reply(c, reportStatusRejected, "short frame")
		return
	}
	var req reportRequest
	if err := json.Unmarshal(buf, &req); err != nil {
		r.reply(c, reportStatusRejected, "bad request")
		return
	}
	status, reason := r.process(req)
	r.reply(c, status, reason)
}

func (r *reportRelay) reply(c net.Conn, status byte, reason string) {
	// 写 deadline 独立于读 deadline：投递（含 hc 子进程）可能占满读窗口。
	_ = c.SetWriteDeadline(time.Now().Add(reportDrain))
	if len(reason) > reportMaxReasonLen {
		reason = reason[:reportMaxReasonLen]
	}
	_, _ = c.Write(append([]byte{status}, reason...))
}

// process 按定稿顺序校验并投递：目标必须等于父地址 → 文本清洗 → 截断 →
// 限流 → 调用 hc send。返回 1 字节状态与简短原因。
func (r *reportRelay) process(req reportRequest) (byte, string) {
	if req.To != r.parent {
		return reportStatusRejected, "non-parent address"
	}
	text := sanitizeReportText(req.Text)
	if strings.TrimSpace(text) == "" {
		return reportStatusRejected, "empty text"
	}
	if !r.takeToken() {
		return reportStatusRejected, "rate limited"
	}
	if err := r.deliver(text); err != nil {
		return reportStatusFailed, fmt.Sprintf("delivery failed: %v", err)
	}
	return reportStatusOK, "ok"
}

// takeToken 实现令牌桶：容量 reportBurstTokens、每 reportRefillEvery 补 1、
// 每会话上限 reportSessionMax 条；超限返回 false。
func (r *reportRelay) takeToken() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if elapsed := now.Sub(r.lastRef); elapsed >= reportRefillEvery {
		add := int(elapsed / reportRefillEvery)
		r.tokens += add
		if r.tokens > reportBurstTokens {
			r.tokens = reportBurstTokens
		}
		r.lastRef = now
	}
	if r.sent >= reportSessionMax || r.tokens <= 0 {
		return false
	}
	r.tokens--
	r.sent++
	return true
}

// deliver 以固定 argv 调用 hc send：不经 shell，环境为外层环境，
// stdout/stderr 丢弃；文本以 "-" 开头时前置一个空格。
func (r *reportRelay) deliver(text string) error {
	if strings.HasPrefix(text, "-") {
		text = " " + text
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportHcTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.hcPath, "send", r.parent, text)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

// sanitizeReportText 做 UTF-8 规范化（非法字节序列剔除）、剔除除 \n\t 外
// 的 C0/C1 控制字符（含 ESC 与 \r），并在超过 reportMaxTextBytes 字节时按
// 字符边界截断后追加截断标记。
func sanitizeReportText(s string) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > reportMaxTextBytes {
		out = truncateUTF8Bytes(out, reportMaxTextBytes) + reportTruncSuffix
	}
	return out
}

// truncateUTF8Bytes 按字节上限截断 s，保证截断点落在 UTF-8 字符边界。
func truncateUTF8Bytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
