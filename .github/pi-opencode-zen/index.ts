import {
	type Api,
	type AssistantMessageEventStream,
	type Context,
	streamSimpleAnthropic,
	streamSimpleGoogle,
	streamSimpleOpenAICompletions,
	streamSimpleOpenAIResponses,
	type Model,
	type SimpleStreamOptions,
} from "@earendil-works/pi-ai";
import type { ExtensionAPI, ProviderModelConfig } from "@earendil-works/pi-coding-agent";
import { randomBytes } from "node:crypto";

const BASE_URL = "https://opencode.ai/zen/v1";
const MODELS_DEV_URL = "https://models.dev/api.json";
const API_KEY = "public";

interface EndpointConfig {
	api: "anthropic-messages" | "google-generative-ai" | "openai-completions" | "openai-responses";
	baseUrl: string;
}

interface ModelsDevModelInfo {
	status?: string | null;
	name?: string | null;
	reasoning?: boolean | null;
	input?: Array<string> | null;
	cost?: {
		input?: number | null;
		output?: number | null;
		cache_read?: number | null;
		cache_write?: number | null;
	} | null;
	contextWindow?: number | null;
	maxTokens?: number | null;
	thinkingLevelMap?: Record<string, string | null> | null;
}

interface UpstreamModelListResponse {
	data?: Array<{ id?: string }>;
}

interface BootstrapState {
	modelIds: string[];
	modelsDevInfo: Record<string, ModelsDevModelInfo>;
}

type SupportedInput = "text" | "image";

type ModelRecord = ProviderModelConfig & {
	id: string;
	name: string;
	reasoning: boolean;
	input: SupportedInput[];
	cost: {
		input: number;
		output: number;
		cacheRead: number;
		cacheWrite: number;
	};
	contextWindow: number;
	maxTokens: number;
};

let bootstrapPromise: Promise<BootstrapState> | undefined;

function randomHex(length: number): string {
	const byteLength = Math.ceil(length / 2);
	return randomBytes(byteLength).toString("hex").slice(0, length);
}

function opencodeHeaders(): Record<string, string> {
	return {
		"User-Agent": "opencode/latest/1.17.15/cli",
		"x-opencode-client": "cli",
		"x-opencode-session": randomHex(26),
		"x-opencode-project": randomHex(26),
		"x-opencode-request": randomHex(26),
	};
}

function isDeprecated(model: ModelsDevModelInfo | undefined): boolean {
	return model?.status === "deprecated";
}

function isFreeModelId(id: string): boolean {
	return id.toLowerCase().includes("free");
}

function toSupportedInput(value: string): value is SupportedInput {
	return value === "text" || value === "image";
}

function buildModelRecord(id: string, info?: ModelsDevModelInfo): ModelRecord {
	const input = (info?.input ?? ["text"]).filter(toSupportedInput);
	return {
		id,
		name: info?.name?.trim() || id,
		reasoning: info?.reasoning ?? true,
		input,
		cost: {
			input: info?.cost?.input ?? 0,
			output: info?.cost?.output ?? 0,
			cacheRead: info?.cost?.cache_read ?? 0,
			cacheWrite: info?.cost?.cache_write ?? 0,
		},
		contextWindow: info?.contextWindow ?? 128000,
		maxTokens: info?.maxTokens ?? 4096,
	};
}

function resolveEndpoint(modelId: string): EndpointConfig {
	if (modelId.startsWith("claude-")) {
		return { api: "anthropic-messages", baseUrl: BASE_URL };
	}
	if (modelId.startsWith("gemini-")) {
		return { api: "google-generative-ai", baseUrl: BASE_URL };
	}
	if (modelId.startsWith("gpt-")) {
		return { api: "openai-responses", baseUrl: BASE_URL };
	}
	return { api: "openai-completions", baseUrl: BASE_URL };
}

async function fetchUpstreamModelIds(): Promise<string[] | undefined> {
	try {
		const response = await fetch(`${BASE_URL}/models`, {
			headers: opencodeHeaders(),
		});
		if (!response.ok) return undefined;
		const json = (await response.json()) as UpstreamModelListResponse;
		return (json.data ?? [])
			.map((entry) => entry.id?.trim())
			.filter((id): id is string => Boolean(id));
	} catch {
		return undefined;
	}
}

async function fetchModelsDevInfo(): Promise<Record<string, ModelsDevModelInfo> | undefined> {
	try {
		const response = await fetch(MODELS_DEV_URL);
		if (!response.ok) return undefined;
		const json = (await response.json()) as {
			opencode?: { models?: Record<string, ModelsDevModelInfo> };
		};
		return json.opencode?.models;
	} catch {
		return undefined;
	}
}

async function loadBootstrapState(): Promise<BootstrapState> {
	if (!bootstrapPromise) {
		bootstrapPromise = (async () => {
			const [upstreamModelIds, modelsDevInfo] = await Promise.all([fetchUpstreamModelIds(), fetchModelsDevInfo()]);
			const ids = new Set<string>();
			for (const id of upstreamModelIds ?? []) ids.add(id);
			for (const id of Object.keys(modelsDevInfo ?? {})) ids.add(id);

			const modelIds = [...ids]
				.filter((id) => isFreeModelId(id))
				.filter((id) => !isDeprecated(modelsDevInfo?.[id]));

			return {
				modelIds,
				modelsDevInfo: modelsDevInfo ?? {},
			};
		})();
	}
	return bootstrapPromise;
}

function getVisibleModels(modelIds: string[], modelsDevInfo: Record<string, ModelsDevModelInfo>): ProviderModelConfig[] {
	return modelIds.map((id) => {
		const model = buildModelRecord(id, modelsDevInfo[id]);
		return {
			id: model.id,
			name: model.name,
			reasoning: model.reasoning,
			input: model.input,
			cost: { ...model.cost },
			contextWindow: model.contextWindow,
			maxTokens: model.maxTokens,
		};
	});
}

function streamOpencodeZen(
	model: Model<Api>,
	context: Context,
	options?: SimpleStreamOptions,
): AssistantMessageEventStream {
	const endpoint = resolveEndpoint(model.id);

	if (model.provider !== "opencode-zen") {
		return streamSimpleOpenAICompletions(model as Model<"openai-completions">, context, options);
	}

	const wrappedModel = {
		...model,
		api: endpoint.api,
		baseUrl: endpoint.baseUrl,
	} as Model<Api>;

	const wrappedOptions: SimpleStreamOptions = {
		...options,
		headers: { ...opencodeHeaders(), ...options?.headers },
	};

	switch (endpoint.api) {
		case "anthropic-messages":
			return streamSimpleAnthropic(wrappedModel as Model<"anthropic-messages">, context, wrappedOptions);
		case "google-generative-ai":
			return streamSimpleGoogle(wrappedModel as Model<"google-generative-ai">, context, wrappedOptions);
		case "openai-responses":
			return streamSimpleOpenAIResponses(wrappedModel as Model<"openai-responses">, context, wrappedOptions);
		case "openai-completions":
		default:
			return streamSimpleOpenAICompletions(wrappedModel as Model<"openai-completions">, context, wrappedOptions);
	}
}

export default async function (pi: ExtensionAPI): Promise<void> {
	const { modelIds, modelsDevInfo } = await loadBootstrapState();
	const visibleModels = getVisibleModels(modelIds, modelsDevInfo);

	pi.registerProvider("opencode-zen", {
		baseUrl: BASE_URL,
		apiKey: API_KEY,
		api: "openai-completions",
		streamSimple: streamOpencodeZen,
		models: visibleModels,
	});
}
