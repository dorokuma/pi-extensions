// Prism slash commands for Pi.
// /quota  → 上游套餐用量（prism quota）
// /usage  → 本地 token 账本（prism usage）
// 结果进会话滚动区，不送给模型。

import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import type { AutocompleteItem, Component } from "@earendil-works/pi-tui";
import { truncateToWidth } from "@earendil-works/pi-tui";
import { type ExtensionAPI, type ExtensionCommandContext, type Theme } from "@earendil-works/pi-coding-agent";

const ENTRY_TYPE = "prism-report";
const MAX_CHARS = 64_000;
const QUOTA_TIMEOUT_MS = 20_000;
const USAGE_TIMEOUT_MS = 15_000;

type Kind = "quota" | "usage";

interface ReportData {
	kind: Kind;
	title: string;
	text: string;
	at: number;
}

const USAGE_PRESETS = ["models", "keys", "accounts", "providers", "days", "hours", "errors"] as const;

export default function (pi: ExtensionAPI) {
	pi.registerEntryRenderer<ReportData>(ENTRY_TYPE, (entry, _opts, theme) => {
		const data = entry.data ?? { kind: "quota" as const, title: "prism", text: "", at: 0 };
		return new StaticReport(data, theme);
	});

	pi.registerCommand("quota", {
		description: "查上游套餐用量（prism quota）",
		getArgumentCompletions: (prefix) =>
			filterCompletions(prefix, [
				{ value: "--provider", label: "--provider", description: "只看这个 provider" },
				{ value: "--json", label: "--json", description: "输出 JSON" },
			]),
		handler: async (args, ctx) => {
			await runAndShow(pi, ctx, "quota", ["quota", ...splitArgs(args)], QUOTA_TIMEOUT_MS, "套餐用量");
		},
	});

	pi.registerCommand("usage", {
		description: "查本地 token 账本（prism usage）",
		getArgumentCompletions: (prefix) =>
			filterCompletions(prefix, [
				...USAGE_PRESETS.map((p) => ({ value: p, label: p, description: `preset: ${p}` })),
				{ value: "--since", label: "--since", description: "起始，如 7d" },
				{ value: "--json", label: "--json", description: "输出 JSON" },
			]),
		handler: async (args, ctx) => {
			const extra = splitArgs(args);
			if (extra.some((t) => t === "--watch" || t.startsWith("--watch="))) {
				ctx.ui.notify("/usage 不支持 --watch，只做一次查询", "warning");
				return;
			}
			await runAndShow(pi, ctx, "usage", ["usage", ...extra], USAGE_TIMEOUT_MS, "本地用量");
		},
	});
}

async function runAndShow(
	pi: ExtensionAPI,
	ctx: ExtensionCommandContext,
	kind: Kind,
	argv: string[],
	timeoutMs: number,
	title: string,
) {
	if (ctx.hasUI) {
		ctx.ui.setStatus("prism", `正在查 ${title}…`);
	}
	const result = await runPrism(argv, timeoutMs);
	if (ctx.hasUI) {
		ctx.ui.setStatus("prism", undefined);
	}

	const text = result.text || `(无输出, exit ${result.code})`;
	pi.appendEntry<ReportData>(ENTRY_TYPE, {
		kind,
		title,
		text,
		at: Date.now(),
	});
	if (result.code !== 0 && ctx.hasUI) {
		ctx.ui.notify(`${title}失败（exit ${result.code}）`, "error");
	}
}

function resolvePrism(): string {
	const env = process.env.PRISM_BIN;
	if (env && existsSync(env)) {
		return env;
	}
	if (existsSync("/usr/local/bin/prism")) {
		return "/usr/local/bin/prism";
	}
	return "prism";
}

function runPrism(argv: string[], timeoutMs: number): Promise<{ code: number; text: string }> {
	return new Promise((resolve) => {
		const bin = resolvePrism();
		const child = spawn(bin, argv, {
			stdio: ["ignore", "pipe", "pipe"],
			// prism 只在 TTY 上默认着色（wantColor 的 ModeCharDevice 检测）；
			// 这里用管道捕获，必须用 CLICOLOR_FORCE=1 显式要求彩色卡片。
			// 保留继承的 process.env（含 PATH），只追加这一个变量。
			env: { ...process.env, CLICOLOR_FORCE: "1" },
		});
		let out = "";
		let err = "";
		let settled = false;
		const finish = (code: number, text: string) => {
			if (settled) {
				return;
			}
			settled = true;
			clearTimeout(timer);
			resolve({ code, text });
		};
		const timer = setTimeout(() => {
			child.kill("SIGTERM");
			finish(124, `prism ${argv.join(" ")} 超时（${timeoutMs}ms）`);
		}, timeoutMs);
		child.stdout.on("data", (buf: Buffer) => {
			out += buf.toString("utf8");
			if (out.length > MAX_CHARS) {
				out = out.slice(0, MAX_CHARS) + "\n…截断";
			}
		});
		child.stderr.on("data", (buf: Buffer) => {
			err += buf.toString("utf8");
			if (err.length > MAX_CHARS) {
				err = err.slice(0, MAX_CHARS) + "\n…截断";
			}
		});
		child.on("error", (e) => {
			finish(127, `无法启动 prism（${bin}）: ${e.message}`);
		});
		child.on("close", (code) => {
			const text = [out.trimEnd(), err.trimEnd()].filter(Boolean).join("\n");
			finish(code ?? 1, text || `(prism 无输出, exit ${code})`);
		});
	});
}

function splitArgs(raw: string): string[] {
	const out: string[] = [];
	const re = /"([^"]*)"|'([^']*)'|(\S+)/g;
	let m: RegExpExecArray | null;
	while ((m = re.exec(raw))) {
		out.push(m[1] ?? m[2] ?? m[3] ?? "");
	}
	return out.filter(Boolean);
}

function filterCompletions(prefix: string, items: AutocompleteItem[]): AutocompleteItem[] | null {
	const p = prefix.trim();
	const hit = p ? items.filter((i) => i.value.startsWith(p)) : items;
	return hit.length > 0 ? hit : null;
}

function reportLines(data: ReportData): string[] {
	return keepSgrSequences(data.text).replace(/\r/g, "").split("\n");
}

// keepSgrSequences 只保留 SGR 颜色序列（以 "m" 结尾的 CSI，如
// ESC[38;2;…m / ESC[38;5;…m / ESC[0m）原样通过，剥掉其余控制序列：
// OSC / APC 载荷、非 m 结尾的 CSI（光标移动、清屏等）以及其它 ESC 序列。
// pi-tui 的 Text / wrapTextWithAnsi / truncateToWidth 都是 ANSI-aware 的，
// 颜色序列进入行后会正常渲染与截断；其余控制序列不得进入行。
function keepSgrSequences(text: string): string {
	if (!text.includes("\x1b")) {
		return text;
	}
	let out = "";
	let i = 0;
	while (i < text.length) {
		if (text[i] !== "\x1b") {
			out += text[i];
			i++;
			continue;
		}
		const next = text[i + 1];
		if (next === "[") {
			// CSI：ESC [ 参数字节（0x30–0x3F）/ 中间字节（0x20–0x2F），
			// 最后跟一个 final byte。只留 final "m"（SGR）。
			let j = i + 2;
			while (j < text.length && /[\x20-\x3f]/.test(text[j])) {
				j++;
			}
			if (j < text.length && text[j] === "m") {
				out += text.slice(i, j + 1);
				i = j + 1;
			} else {
				// 非 SGR 的 CSI（光标/清屏等）或被截断的 CSI：整段丢弃。
				i = j < text.length ? j + 1 : j;
			}
			continue;
		}
		if (next === "]" || next === "_") {
			// OSC / APC 载荷：直到 BEL（0x07）或 ST（ESC \），含终止符。
			let j = i + 2;
			while (j < text.length) {
				if (text[j] === "\x07") {
					j++;
					break;
				}
				if (text[j] === "\x1b" && text[j + 1] === "\\") {
					j += 2;
					break;
				}
				j++;
			}
			i = j;
			continue;
		}
		// 其它 ESC 序列：普通两字符转义（ESC + final byte）；若紧跟的是
		// 中间字节（0x20–0x2F，如 ESC ( B 指定字符集），再多吞一个 final。
		let j = i + 2;
		if (next !== undefined && /[\x20-\x2f]/.test(next)) {
			j = i + 3;
		}
		i = Math.min(j, text.length);
	}
	return out;
}

class StaticReport implements Component {
	constructor(
		private data: ReportData,
		private theme: Theme,
	) {}

	invalidate(): void {}

	render(width: number): string[] {
		const lines = withUsageIndent(reportLines(this.data));
		return lines.map((line) => truncateToWidth(line, width));
	}
}

// usage 正文默认左缩进两格。quota 原文若还没缩进，这里补上，两边对齐。
function withUsageIndent(lines: string[]): string[] {
	return lines.map((line) => {
		if (line === "" || line.startsWith("  ")) {
			return line;
		}
		return "  " + line;
	});
}
