// magpie-image: register the local magpie gateway as a pi *image* provider, so
// codemode's `models.generateImages()` works without any OpenRouter credential.
//
// Why an extension: pi's models.json cannot declare image models, so the image
// model plus its `generateImages` implementation must come from a provider
// registration. The provider id is deliberately `magpie-image` and NOT `magpie`:
// the legacy registration form replaces every chat/image/classifier model of the
// same provider id when a `models` list is supplied, and `magpie` already holds
// the chat models configured in models.json. Registering under a distinct id
// leaves that provider untouched.
//
// Gateway facts this relies on (verified against the running gateway):
//   POST http://127.0.0.1:3425/v1/images/generations
//   Authorization: Bearer <apiKey>
//   request  { "model": "<id>", "prompt": "<text>" }
//   response { "created": <int>, "data": [{ "b64_json": "<base64 png>", ... }], "model": "<id>", "usage": {...} }

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import type {
	AssistantImages,
	ImageModel,
	ImageApi,
	ImagesContext,
	ImagesOptions,
	Usage,
} from "@earendil-works/pi-ai";

/** Provider id: kept distinct from the chat provider "magpie" in models.json. */
export const PROVIDER_ID = "magpie-image";

/** Image API discriminator used to key the `images` implementation map. */
export const IMAGE_API = "magpie-images";

/** Defaults come from magpie's own settings.json `imageGen` and its local endpoint. */
export const DEFAULT_IMAGE_MODEL_ID = "workbuddy-ai/gpt-image-2.5-sunburst";
export const DEFAULT_IMAGE_BASE_URL = "http://127.0.0.1:3425/v1";

/** Literal gateway key; magpie accepts any bearer token locally, this one is not a secret. */
const GATEWAY_KEY = "magpie";

const ZERO_COST = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };

function resolveModelId(): string {
	const override = process.env.MAGPIE_IMAGE_MODEL?.trim();
	return override && override.length > 0 ? override : DEFAULT_IMAGE_MODEL_ID;
}

function resolveBaseUrl(): string {
	const override = process.env.MAGPIE_IMAGE_BASE_URL?.trim();
	const raw = override && override.length > 0 ? override : DEFAULT_IMAGE_BASE_URL;
	return raw.replace(/\/+$/, "");
}

/** The prompt is every text block of the context; reference images are not sent by this endpoint. */
function buildPrompt(context: ImagesContext): string {
	return context.input
		.filter((block) => block.type === "text")
		.map((block) => block.text)
		.join("\n")
		.trim();
}

function parseUsage(raw: unknown, model: ImageModel<ImageApi>): Usage | undefined {
	if (typeof raw !== "object" || raw === null) return undefined;
	const record = raw as Record<string, unknown>;
	const num = (value: unknown): number => (typeof value === "number" && Number.isFinite(value) ? value : 0);
	const input = num(record.input_tokens ?? record.prompt_tokens);
	const output = num(record.output_tokens ?? record.completion_tokens);
	const totalTokens = num(record.total_tokens) || input + output;
	const cost = {
		input: (model.cost.input / 1_000_000) * input,
		output: (model.cost.output / 1_000_000) * output,
		cacheRead: 0,
		cacheWrite: 0,
		total: 0,
	};
	cost.total = cost.input + cost.output;
	return { input, output, cacheRead: 0, cacheWrite: 0, totalTokens, cost };
}

/**
 * Generate images through magpie's OpenAI-compatible `/images/generations`.
 * Never throws: failures come back as an `AssistantImages` with `stopReason: "error"`.
 */
async function generateImages(
	model: ImageModel<ImageApi>,
	context: ImagesContext,
	options?: ImagesOptions,
): Promise<AssistantImages> {
	const result: AssistantImages = {
		api: model.api,
		provider: model.provider,
		model: model.id,
		output: [],
		stopReason: "stop",
		timestamp: Date.now(),
	};

	try {
		const prompt = buildPrompt(context);
		if (prompt.length === 0) {
			throw new Error("No text prompt in the image context");
		}

		const baseUrl = (model.baseUrl || resolveBaseUrl()).replace(/\/+$/, "");
		const url = `${baseUrl}/images/generations`;
		const apiKey = options?.apiKey && options.apiKey.length > 0 ? options.apiKey : GATEWAY_KEY;

		let payload: unknown = { model: model.id, prompt };
		const replaced = await options?.onPayload?.(payload, model);
		if (replaced !== undefined) {
			payload = replaced;
		}

		const fetchImpl = options?.fetch ?? globalThis.fetch;
		const headers: Record<string, string> = {
			"Content-Type": "application/json",
			...(options?.headers as Record<string, string> | undefined),
		};
		if (!Object.keys(headers).some((name) => name.toLowerCase() === "authorization")) {
			headers.Authorization = `Bearer ${apiKey}`;
		}

		const response = await fetchImpl(url, {
			method: "POST",
			headers,
			body: JSON.stringify(payload),
			...(options?.signal ? { signal: options.signal } : {}),
		});

		await options?.onResponse?.(
			{ status: response.status, headers: Object.fromEntries(response.headers.entries()) },
			model,
		);

		if (!response.ok) {
			const body = await response.text().catch(() => "");
			throw new Error(
				`magpie image request failed: HTTP ${response.status}${body ? `: ${body.slice(0, 300)}` : ""}`,
			);
		}

		const json = (await response.json()) as {
			data?: Array<{ b64_json?: unknown; mime_type?: unknown }>;
			usage?: unknown;
			created?: unknown;
		};
		const first = Array.isArray(json.data) ? json.data[0] : undefined;
		const data = first && typeof first.b64_json === "string" ? first.b64_json : undefined;
		if (!data || data.length === 0) {
			throw new Error("magpie image response carried no base64 image data");
		}

		result.output.push({ type: "image", data, mimeType: "image/png" });
		const usage = parseUsage(json.usage, model);
		if (usage) {
			result.usage = usage;
		}
		return result;
	} catch (error) {
		result.stopReason = options?.signal?.aborted ? "aborted" : "error";
		result.errorMessage = error instanceof Error ? error.message : String(error);
		return result;
	}
}

export default function magpieImage(pi: ExtensionAPI): void {
	const modelId = resolveModelId();
	const baseUrl = resolveBaseUrl();

	pi.registerProvider(PROVIDER_ID, {
		name: "magpie (images)",
		apiKey: GATEWAY_KEY,
		baseUrl,
		models: [
			{
				type: "image",
				id: modelId,
				name: `${modelId} (magpie)`,
				api: IMAGE_API,
				baseUrl,
				input: ["text", "image"],
				output: ["image"],
				cost: { ...ZERO_COST },
			},
		],
		images: {
			[IMAGE_API]: { generateImages },
		},
	});
}
