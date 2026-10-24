/**
 * auto-continue 计时器取消语义回归台架（node:test）。
 *
 * 被测对象是**真实**扩展：读取 `extensions/auto-continue/src/auto-continue.ts`
 * （可用 `AUTO_CONTINUE_SRC=<path>` 指向只读基线副本，做「修复前红 / 修复后绿」
 * 对照），用 `typescript.transpileModule` 编成 CJS 后在当前 realm 里执行，
 * 宿主侧只注入 ExtensionAPI / ExtensionContext polyfill、假 fs（配置写不到真实
 * home）、假 env、假 scheduler。扩展内部的判定/取消逻辑一行不复制，用例只驱动
 * `session_start` / `agent_end` / `agent_settled` / `onTerminalInput` 这些真实入口。
 *
 * 用法（README『自检』同口径）：
 *   node --test extensions/auto-continue/test/auto-continue-timer-race.test.mjs
 *
 * 单线程事实：TS 扩展没有并行线程，ESC 与 timer 回调不会同时跑。用例覆盖的是两条
 * **假设性/防御性**交错——① 已 clearTimeout（已排队）的旧回调被台架强制重放；
 * ② 回调内部调用宿主方法（isIdle / hasPendingMessages / getEditorText / setStatus）
 * 时由同步 hook 重入裸 ESC 处理。两者都由台架确定性强制，不 sleep、不赌调度；它们
 * **不是** Pi 1.4.0 在线 Node 事件调度中已发生或已证明可达的故障，用例只固化与 Go
 * 版一致的不变量。
 */

import assert from "node:assert/strict";
import { readFileSync as readFile } from "node:fs";
import * as nodePath from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const HERE = nodePath.dirname(fileURLToPath(import.meta.url));
const DEFAULT_SOURCE = nodePath.resolve(HERE, "../src/auto-continue.ts");
const SOURCE = process.env.AUTO_CONTINUE_SRC
  ? nodePath.resolve(process.env.AUTO_CONTINUE_SRC)
  : DEFAULT_SOURCE;

/** DEFAULT_DELAY_MS；压缩看门狗是 300_000ms，用它把两类 timer 分开。 */
const CONTINUE_DELAY_MS = 2500;
const CONTINUE_PROMPT = "继续";
const BARE_ESC = "\u001b";

const CONTINUE_WORTHY_ASSISTANT = {
  role: "assistant",
  stopReason: "error",
  errorMessage: "HTTP 502 bad gateway",
};

/** 假 scheduler：记录每次 setTimeout，允许「重放已排队（甚至已 clearTimeout）的回调」。 */
function createFakeTimers() {
  let nextId = 0;
  const entries = new Map();
  return {
    setTimeout(fn, delay) {
      const id = ++nextId;
      entries.set(id, { id, fn, delay, cleared: false, fired: false });
      return id;
    },
    clearTimeout(id) {
      const entry = entries.get(id);
      if (entry) entry.cleared = true;
    },
    /** 重放一个已排队/已取消的回调（模拟「已排出但仍在队列里」的旧 callback）。 */
    forceRun(id) {
      const entry = entries.get(id);
      assert.ok(entry, `timer ${id} must have been armed`);
      entry.fired = true;
      entry.fn();
    },
    get(id) {
      return entries.get(id);
    },
    /** 当前仍在等待的「继续」timer（不含压缩看门狗，也不含已经触发的）。 */
    pendingContinue() {
      return [...entries.values()].filter(
        (e) => e.delay === CONTINUE_DELAY_MS && !e.cleared && !e.fired,
      );
    },
    /** 最近一个「继续」timer（含已被 clear 的，便于断言引用是否被旧回调清掉/保留）。 */
    lastContinue() {
      const list = [...entries.values()].filter((e) => e.delay === CONTINUE_DELAY_MS);
      return list[list.length - 1];
    },
  };
}

function loadFactory(timers) {
  const js = ts.transpileModule(readFile(SOURCE, "utf8"), {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS,
      target: ts.ScriptTarget.ES2022,
      esModuleInterop: true,
    },
    fileName: "auto-continue.ts",
  }).outputText;

  const module = { exports: {} };
  const requireShim = (id) => {
    if (id === "node:fs") {
      return {
        // 配置文件永远不存在 → loadPersist 走默认值并落回写（写也是 no-op）。
        readFileSync() {
          throw Object.assign(new Error("ENOENT"), { code: "ENOENT" });
        },
        writeFileSync() {},
        mkdirSync() {},
      };
    }
    if (id === "node:os") return { homedir: () => "/tmp/auto-continue-fake-home" };
    if (id === "node:path") return nodePath;
    throw new Error(`unexpected require: ${id}`);
  };

  const run = new Function(
    "exports",
    "require",
    "module",
    "process",
    "setTimeout",
    "clearTimeout",
    "console",
    js,
  );
  run(
    module.exports,
    requireShim,
    module,
    { env: {} },
    (fn, delay) => timers.setTimeout(fn, delay),
    (id) => timers.clearTimeout(id),
    { log() {}, warn() {}, error() {} },
  );

  const factory = module.exports.default;
  assert.equal(typeof factory, "function", "扩展默认导出必须是工厂函数");
  return factory;
}

/** 一套隔离台架：假宿主 + 假 scheduler + 真实扩展工厂。 */
function createRig() {
  const timers = createFakeTimers();
  const record = { send: [], notify: [], status: [] };
  const handlers = new Map();
  const commands = new Map();
  const host = { idle: true, pending: false, draft: "", onIsIdle: undefined, onHasPendingMessages: undefined, onGetEditorText: undefined, onSetStatus: undefined };
  let terminalHandler;

  const ctx = {
    ui: {
      setStatus(key, text) {
        record.status.push({ key, text });
        host.onSetStatus?.();
      },
      notify(message, level) {
        record.notify.push({ message, level });
      },
      onTerminalInput(handler) {
        terminalHandler = handler;
        return () => {
          if (terminalHandler === handler) terminalHandler = undefined;
        };
      },
      getEditorText() {
        host.onGetEditorText?.();
        return host.draft;
      },
    },
    isIdle() {
      host.onIsIdle?.();
      return host.idle;
    },
    hasPendingMessages() {
      host.onHasPendingMessages?.();
      return host.pending;
    },
  };

  const pi = {
    on(event, handler) {
      handlers.set(event, handler);
    },
    registerCommand(name, definition) {
      commands.set(name, definition);
    },
    sendUserMessage(text) {
      record.send.push(text);
    },
  };

  loadFactory(timers)(pi);

  const rig = {
    timers,
    record,
    host,
    startSession() {
      handlers.get("session_start")({}, ctx);
      assert.equal(typeof terminalHandler, "function", "session_start 必须注册裸 ESC 监听");
    },
    /** 一轮瞬时错误的 agent_end + agent_settled（真实入口，不伪造内部状态）。 */
    async settleWithError(assistant = CONTINUE_WORTHY_ASSISTANT) {
      handlers.get("agent_end")({ messages: [assistant] });
      await handlers.get("agent_settled")({}, ctx);
    },
    /** 裸 ESC：走真实 onTerminalInput handler（不 consume）。 */
    pressEsc() {
      assert.equal(typeof terminalHandler, "function", "裸 ESC 监听未注册");
      return terminalHandler(BARE_ESC);
    },
    async command(args) {
      await commands.get("auto-continue").handler(args, ctx);
    },
    lastNotify() {
      return record.notify[record.notify.length - 1];
    },
    cancelNotifies() {
      return record.notify.filter((n) => /已取消本轮自动继续/.test(n.message));
    },
    forceRun(id) {
      timers.forceRun(id);
    },
  };
  return rig;
}

/** 装一轮 pending「继续」并返回它的 timer id。 */
async function armOneContinue(rig) {
  await rig.settleWithError();
  const armed = rig.timers.lastContinue();
  assert.ok(armed, "应当在 agent_settled 里排出一个「继续」timer");
  assert.equal(rig.timers.pendingContinue().length, 1, "同一时刻只应有一个 pending「继续」");
  return armed.id;
}

test("T1 已排出但被取消的旧回调不得动新一代 timer：重放后 ESC 仍能取消新代，最终 send 为 0", async () => {
  const rig = createRig();
  rig.startSession();

  const t1 = await armOneContinue(rig);
  // R2：计时中按裸 ESC → 取消本轮（不置 userCancelled）。
  rig.pressEsc();
  assert.equal(rig.timers.get(t1).cleared, true, "T1 必须被 clearTimeout");
  assert.equal(rig.cancelNotifies().length, 1, "取消通知只出现一次");

  // 装上 T2（新一代）。
  const t2 = await armOneContinue(rig);
  assert.notEqual(t2, t1, "T2 必须是不同代的新 timer");

  // 重放「已排出但仍被调度」的旧 T1 回调：代号不符，必须什么都不碰。
  rig.forceRun(t1);
  assert.deepEqual(rig.record.send, [], "旧回调不得发送");
  assert.equal(rig.timers.get(t2).cleared, false, "旧回调不得清掉 T2 的 timer 引用");

  // 新一代仍可被裸 ESC 取消（说明它的代号/相位没被旧回调破坏）。
  rig.pressEsc();
  assert.equal(rig.cancelNotifies().length, 2, "T2 必须仍能被 ESC 取消并报告取消");
  assert.equal(rig.timers.get(t2).cleared, true, "T2 必须被 clearTimeout");

  // 强制运行已排的 T2 回调：取消先赢，一次都不发。
  rig.forceRun(t2);
  assert.deepEqual(rig.record.send, [], "已取消的 T2 回调不得发送");
});

test("T2 宿主前置检查期间重入裸 ESC：本代 Checking 仍可取消，send 为 0、streak 不增、取消通知准确", async (t) => {
  for (const hookName of ["onIsIdle", "onHasPendingMessages", "onGetEditorText"]) {
    await t.test(`重入点 ${hookName}`, async () => {
      const rig = createRig();
      rig.startSession();
      const armed = await armOneContinue(rig);

      let reentered = 0;
      rig.host[hookName] = () => {
        if (reentered > 0) return;
        reentered += 1;
        rig.pressEsc();
      };

      rig.forceRun(armed);
      assert.equal(reentered, 1, "重入 hook 必须真的被执行到");
      assert.deepEqual(rig.record.send, [], "取消先赢：一次都不能发");
      assert.equal(rig.cancelNotifies().length, 1, "取消通知必须准确出现一次");
      assert.equal(rig.timers.pendingContinue().length, 0, "取消后不应残留 pending「继续」");

      await rig.command("status");
      assert.match(rig.lastNotify().message, /streak=0\/20/, "streak 不得因取消的轮次增加");
    });
  }
});

test("T3a 取消先赢（armed 窗口内重入 ESC）：timer 不被装上，重放也不发送", async () => {
  const rig = createRig();
  rig.startSession();

  // armed 相位覆盖 status/notify 这段宿主调用：这里重入 ESC 取消。
  let reentered = 0;
  rig.host.onSetStatus = () => {
    if (reentered > 0) return;
    reentered += 1;
    rig.pressEsc();
  };

  await rig.settleWithError();
  assert.equal(reentered, 1, "setStatus 重入必须命中 armed 窗口");
  assert.equal(rig.cancelNotifies().length, 1, "取消通知必须出现一次");
  assert.equal(rig.timers.lastContinue(), undefined, "取消已先赢：不得再装 timer");
  assert.deepEqual(rig.record.send, []);

  rig.host.onSetStatus = undefined;
  await rig.settleWithError();
  const second = rig.timers.lastContinue();
  assert.ok(second, "取消窗口之后的新一轮仍应正常排程（不误伤后续）");
  rig.forceRun(second.id);
  assert.deepEqual(rig.record.send, [CONTINUE_PROMPT], "取消之后的新一轮照常发送");
});

test("T3b 发送已 commit：之后的 ESC 不得谎称取消本轮，也不再补发", async () => {
  const rig = createRig();
  rig.startSession();
  const armed = await armOneContinue(rig);

  // 正常到期 → committed → 发送。
  rig.forceRun(armed);
  assert.deepEqual(rig.record.send, [CONTINUE_PROMPT], "到期应发送一条「继续」");

  // 已 commit 且 pi 仍 idle、无 pending：ESC 既不能取消已发出的消息，也不得声称取消。
  const before = rig.record.notify.length;
  rig.pressEsc();
  assert.equal(rig.record.notify.length, before, "已 commit 后 ESC 不得产生「取消本轮」通知");

  // 真实运行中的 ESC 只能走 markUserAbort（「已按 esc 中止」），同样不能声称取消了计时。
  rig.host.idle = false;
  rig.pressEsc();
  const last = rig.lastNotify();
  assert.match(last.message, /已按 esc 中止/, "运行中的 ESC 走真实中止路径");
  assert.doesNotMatch(last.message, /已取消本轮自动继续/, "不得声称取消了已 commit 的发送");
  assert.deepEqual(rig.record.send, [CONTINUE_PROMPT], "已 commit 的发送不会被回滚或补发");
});
