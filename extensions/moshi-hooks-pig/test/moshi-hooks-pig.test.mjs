/**
 * moshi-hooks-pig 冒烟回归台架（node:test）。
 *
 * 被测对象是**真实**扩展：读取 extensions/moshi-hooks-pig/src/moshi-hooks.ts，
 * 用 typescript.transpileModule 编成 CJS 后在当前 realm 里执行，宿主侧只注入
 * ExtensionAPI polyfill 与假 ctx；网络侧起一个一次性 Unix socket capture 守护进程，
 * 用 MOSHI_SOCKET_PATH 指过去，逐条收 envelope 断言字段。协议层（250ms 超时、
 * 单连接一 JSON 行、静默失败、session_shutdown 的 flushSends）沿用 pi 版口径。
 *
 * 用法（README『自检』同口径）：
 *   node --test extensions/moshi-hooks-pig/test/moshi-hooks-pig.test.mjs
 *
 * pig 特有断言（相对 pi 版的适配点）：
 *   1. envelope.source === "pi"（手机端只认已注册名单，注释已说明伪装原因）；
 *   2. transcriptPath 指向 pig 会话 jsonl（PIG_EXT_HOST_INFO.agentDir 下的
 *      sessions/<encoded-cwd>/<ts>_<sessionId>.jsonl），绝不落在 ~/.pi 下；
 *   3. herdrPane/herdrWorkspaceId/herdrTabId 照常填充；
 *   4. sessionManager 缺 getSessionFile 时，按 sessionId 在 sessions 根下
 *      跨目录定位真实 transcript。
 */
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import * as nodePath from "node:path";
import { tmpdir } from "node:os";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";
import ts from "typescript";

const require = createRequire(import.meta.url);

const HERE = nodePath.dirname(fileURLToPath(import.meta.url));
const SOURCE = nodePath.resolve(HERE, "../src/moshi-hooks.ts");

/** 编译并在当前 realm 执行被测扩展，返回 default export 的工厂函数。 */
function loadExtension() {
  const source = ts.transpileModule(readFileSync(SOURCE, "utf8"), {
    // 编成 CJS，require("node:*") 由当前 realm 解析
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText;
  const file = nodePath.join(mkdtempSync(nodePath.join(tmpdir(), "moshi-pig-")), "ext.cjs");
  writeFileSync(file, source);
  return require(file).default;
}

/** 一次性 capture 守护进程：每条连接收一行 JSON，按到达顺序记录。 */
function startCapture(socketPath) {
  const received = [];
  const server = createServer((sock) => {
    let buf = "";
    sock.on("data", (chunk) => {
      buf += chunk.toString("utf8");
    });
    sock.on("end", () => {
      const line = buf.trim();
      if (line) received.push(JSON.parse(line));
      sock.end("ok\n");
    });
    sock.on("error", () => {});
  });
  return new Promise((resolve) => {
    server.listen(socketPath, () =>
      resolve({ received, close: () => new Promise((r) => server.close(r)) }),
    );
  });
}

/** 假 ExtensionAPI：只实现本扩展用到的 pi.on。 */
function createFakePi() {
  const handlers = new Map();
  return {
    on(name, fn) {
      handlers.set(name, fn);
    },
    emit(name, event, ctx) {
      return handlers.get(name)?.(event, ctx);
    },
  };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

test("envelope wire format: source is \"pi\", pig transcript path, herdr pane fields", async (t) => {
  const root = mkdtempSync(nodePath.join(tmpdir(), "moshi-pig-home-"));
  const agentDir = nodePath.join(root, "agent");
  const sessionDir = nodePath.join(agentDir, "sessions", "--tmp-pig-moshi--");
  mkdirSync(sessionDir, { recursive: true });
  const sessionId = "01a18249-a0f2-73a3-b0b5-3e14fef1b16b";
  const transcript = nodePath.join(sessionDir, `2026-10-28T11-33-45-842Z_${sessionId}.jsonl`);
  writeFileSync(transcript, '{"type":"session","version":3,"id":"' + sessionId + '","cwd":"/tmp/pig-moshi"}\n');
  t.after(() => rmSync(root, { recursive: true, force: true }));

  const socketPath = nodePath.join(root, "capture.sock");
  const capture = await startCapture(socketPath);
  t.after(() => capture.close());

  process.env.MOSHI_SOCKET_PATH = socketPath;
  process.env.PIG_EXT_HOST_INFO = JSON.stringify({ agentDir, configDirName: ".pig" });
  process.env.HERDR_ENV = "1";
  process.env.HERDR_PANE_ID = "w37:p2D";
  process.env.HERDR_WORKSPACE_ID = "w37";
  process.env.HERDR_TAB_ID = "w37:t2D";

  const load = loadExtension();
  const pi = createFakePi();
  const ctx = {
    cwd: "/tmp/pig-moshi",
    model: { displayName: "group/worker" },
    getContextUsage: () => ({ percent: 25 }),
    getSessionId: () => sessionId,
    // 故意不提供 sessionManager.getSessionFile：走 pig 目录回退
  };
  load(pi);

  pi.emit("session_start", {}, ctx);
  pi.emit("before_agent_start", { prompt: "<user_query>回复 OK</user_query>" }, ctx);
  pi.emit("agent_end", { messages: [{ role: "assistant", content: [{ type: "text", text: "OK" }] }] }, ctx);
  pi.emit("agent_settled", {}, ctx);
  await pi.emit("session_shutdown", {}, ctx);
  await sleep(50);

  assert.equal(capture.received.length, 4, `expected 4 envelopes, got ${capture.received.length}`);

  const [start, prompt, settled, closed] = capture.received;
  // source 伪装：手机端只认已注册名单
  for (const env of capture.received) assert.equal(env.source, "pi");
  assert.equal(start.type, "session.update");
  assert.equal(start.eventName, "SessionStart");
  // 静默状态载体：无 category/title
  assert.equal(start.category, undefined);
  assert.equal(start.title, undefined);
  // transcriptPath 指向 pig 真实会话，不是 ~/.pi
  assert.equal(start.transcriptPath, transcript);
  assert.ok(!start.transcriptPath.includes("/.pi/"), "transcriptPath must not live under ~/.pi");
  assert.equal(start.sessionId, sessionId);
  assert.equal(start.projectName, "pig-moshi");
  assert.equal(start.cwd, "/tmp/pig-moshi");
  assert.equal(start.modelName, "group/worker");
  assert.equal(start.contextRemaining, 75);
  // herdr pane 标识照常填
  assert.equal(start.terminalKind, "herdr");
  assert.equal(start.herdrPane, "w37:p2D");
  assert.equal(start.herdrWorkspaceId, "w37");
  assert.equal(start.herdrTabId, "w37:t2D");
  assert.equal(typeof start.muxEnv, "object");

  // 首问句 → SessionStart + session_started + You: 摘要（剥掉 <user_query> 包装）
  assert.equal(prompt.eventName, "SessionStart");
  assert.equal(prompt.category, "session_started");
  assert.equal(prompt.title, "Pi session started");
  assert.equal(prompt.message, "You: 回复 OK");

  // agent_settled → task_complete
  assert.equal(settled.eventName, "AgentEnd");
  assert.equal(settled.category, "task_complete");
  assert.equal(settled.title, "OK");

  // session_shutdown → session.closed
  assert.equal(closed.type, "session.closed");
  assert.equal(closed.category, "session_ended");
  assert.equal(closed.title, "Pi session ended");

  delete process.env.MOSHI_SOCKET_PATH;
  delete process.env.PIG_EXT_HOST_INFO;
  delete process.env.HERDR_ENV;
  delete process.env.HERDR_PANE_ID;
  delete process.env.HERDR_WORKSPACE_ID;
  delete process.env.HERDR_TAB_ID;
});

test("missing daemon: sends fail silently and the turn is not interrupted", async () => {
  const socketPath = nodePath.join(mkdtempSync(nodePath.join(tmpdir(), "moshi-pig-none-")), "absent.sock");
  process.env.MOSHI_SOCKET_PATH = socketPath;
  const load = loadExtension();
  const pi = createFakePi();
  load(pi);
  const ctx = { cwd: "/tmp/pig-moshi", model: { name: "m" } };
  // 不抛异常即通过；send 静默 resolve
  pi.emit("session_start", {}, ctx);
  await pi.emit("session_shutdown", {}, ctx);
  delete process.env.MOSHI_SOCKET_PATH;
});
