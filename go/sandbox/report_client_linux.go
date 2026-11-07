//go:build linux

package sandbox

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// 沙盒内 hc 受限客户端：ReviewerAgent 预设在 <goExecDir>/bin/hc 建立指向
// sbx 本体的软链，argv[0] 命中 init 分发进入本函数。只放行
// `hc send <addr> <text>` 精确两操作数（经 <goExecDir>/report.sock 提交给
// 沙盒外中继，由中继校验目标并执行真实 hc send）；`hc --help`/`hc --version`
// 打印受限说明退 0；其它任何形态退 126；中继不可用或 PI_SANDBOX_GO_EXEC_DIR
// 为空时 stderr 一行并退 75。真 /root/.local/bin/hc 不进任何 Exec 白名单。
//
// 帧协议与 cmd/sbx/report_relay.go 对齐：4 字节大端长度前缀 +
// JSON {"to","text"}，回包 1 字节状态（0 成功 / 1 拒绝 / 2 投递失败）+ 原因。

// reportSockNameForClient 与 cmd/sbx/report_relay.go 的 reportSockName 一致。
const (
	reportSockNameForClient = "report.sock"
	reportClientDialTimeout = 2 * time.Second
)

func runReportClient(goExecDir string) int {
	usage := func() {
		fmt.Println("sandbox hc: restricted client; only `hc send <addr> <text>` is allowed")
		fmt.Println("Usage: hc send <addr> <text>")
	}
	args := os.Args[1:]
	if len(args) == 1 && (args[0] == "--help" || args[0] == "--version") {
		usage()
		return 0
	}
	// 只接受 `hc send <addr> <text>` 精确两操作数，其它形态一律退 126。
	if len(args) == 3 && args[0] == "send" {
		return reportSend(goExecDir, args[1], args[2])
	}
	usage()
	return 126
}

// reportSend 把单条 send 请求经 report.sock 提交给沙盒外中继。
// 目标地址与文本的校验、清洗、截断、限流全部由中继完成；本端只转送并
// 按回包状态映射退出码：0→0、1 拒绝→1、2 投递失败/不可达→75。
func reportSend(goExecDir, addr, text string) int {
	if goExecDir == "" {
		fmt.Fprintln(os.Stderr, "hc: sandbox report relay unavailable (PI_SANDBOX_GO_EXEC_DIR not set)")
		return 75
	}
	sockPath := filepath.Join(goExecDir, reportSockNameForClient)
	conn, err := net.DialTimeout("unix", sockPath, reportClientDialTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hc: sandbox report relay unavailable: %v\n", err)
		return 75
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(reportClientDialTimeout + 8*time.Second))

	payload, err := json.Marshal(struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}{To: addr, Text: text})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hc: marshal report request failed: %v\n", err)
		return 1
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := conn.Write(append(hdr[:], payload...)); err != nil {
		fmt.Fprintf(os.Stderr, "hc: report send failed: %v\n", err)
		return 75
	}
	resp, err := io.ReadAll(io.LimitReader(conn, 512))
	if err != nil || len(resp) == 0 {
		fmt.Fprintf(os.Stderr, "hc: report send failed: relay closed without reply\n")
		return 75
	}
	reason := string(resp[1:])
	switch resp[0] {
	case 0:
		return 0
	case 1:
		fmt.Fprintf(os.Stderr, "hc: report rejected: %s\n", reason)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "hc: report delivery failed: %s\n", reason)
		return 75
	}
}
