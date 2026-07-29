import * as vscode from "vscode";
import { BridgeProcess } from "../bridge/BridgeProcess";
import { ModelInfo } from "../bridge/protocol";
import { ChatTurnRunner } from "../chat/turnRunner";
import { ContenoxOutput } from "../logging/output";
import { TelemetryLogger } from "../logging/telemetry";

interface ContenoxLanguageModel extends vscode.LanguageModelChatInformation {
  provider?: string;
  modelName: string;
  // Present on current VS Code builds but not yet in @types/vscode. Without a
  // user-selectable, default model the chat model picker stays empty and chat
  // fails with VS Code's "Language model unavailable" on installs that have no
  // other language-model provider (e.g. no GitHub Copilot).
  isUserSelectable?: boolean;
  isDefault?: boolean;
}

const vendor = "contenox";

export function registerLanguageModelProvider(
  bridge: BridgeProcess,
  output: ContenoxOutput,
  telemetry: TelemetryLogger,
): vscode.Disposable {
  const provider = new ContenoxLanguageModelProvider(bridge, output, telemetry);
  telemetry.event("lm.provider.registered", { vendor });
  return vscode.lm.registerLanguageModelChatProvider(vendor, provider);
}

export async function testLanguageModelProvider(output: ContenoxOutput, telemetry: TelemetryLogger): Promise<void> {
  const startedAt = Date.now();
  telemetry.event("lm.test.start", { vendor });
  output.show();
  output.info("Testing VS Code language model provider vendor=contenox");

  const models = await vscode.lm.selectChatModels({ vendor });
  if (models.length === 0) {
    telemetry.warn("lm.test.no_models", { vendor });
    vscode.window.showWarningMessage("No Contenox language models are visible to VS Code.");
    return;
  }

  const model = models[0];
  const cts = new vscode.CancellationTokenSource();
  const timer = setTimeout(() => cts.cancel(), 60000);
  try {
    output.info(`Using Contenox LM model: ${model.name} (${model.id})`);
    const response = await model.sendRequest(
      [vscode.LanguageModelChatMessage.User("Reply with one short sentence that starts with: Contenox LM OK")],
      { justification: "Smoke-test the Contenox language model provider." },
      cts.token,
    );
    let text = "";
    for await (const chunk of response.text) {
      text += chunk;
      if (text.length > 2000) {
        break;
      }
    }
    output.info(`Contenox LM test response:\n${text.trim() || "(empty response)"}`);
    telemetry.event("lm.test.end", {
      modelId: model.id,
      durationMs: Date.now() - startedAt,
      responseChars: text.length,
    });
    vscode.window.showInformationMessage("Contenox LM provider responded. See Contenox output.");
  } catch (error) {
    telemetry.error("lm.test.failed", error);
    vscode.window.showErrorMessage(errorMessage(error));
  } finally {
    clearTimeout(timer);
    cts.dispose();
  }
}

class ContenoxLanguageModelProvider implements vscode.LanguageModelChatProvider<ContenoxLanguageModel> {
  private readonly turns: ChatTurnRunner;

  public constructor(
    private readonly bridge: BridgeProcess,
    output: ContenoxOutput,
    private readonly telemetry: TelemetryLogger,
  ) {
    this.turns = new ChatTurnRunner(bridge, output, telemetry);
  }

  public async provideLanguageModelChatInformation(
    _options: vscode.PrepareLanguageModelChatModelOptions,
    _token: vscode.CancellationToken,
  ): Promise<ContenoxLanguageModel[]> {
    try {
      const state = await this.bridge.ensureStarted();
      const client = this.bridge.currentClient;
      if (!client || !state.initialize.capabilities.models) {
        return [fallbackModel(state.health.defaultProvider, state.health.defaultModel)];
      }
      const models = await client.listModels();
      const infos = dedupeModels(models.models.map((model) => modelInfoToLanguageModel(model)));
      if (infos.length > 0) {
        markDefaultModel(infos, state.health.defaultProvider, state.health.defaultModel);
        this.telemetry.event("lm.models.listed", { count: infos.length });
        return infos;
      }
      return [fallbackModel(state.health.defaultProvider, state.health.defaultModel)];
    } catch (error) {
      this.telemetry.error("lm.models.failed", error);
      return [fallbackModel(undefined, undefined)];
    }
  }

  public async provideLanguageModelChatResponse(
    model: ContenoxLanguageModel,
    messages: readonly vscode.LanguageModelChatRequestMessage[],
    _options: vscode.ProvideLanguageModelChatResponseOptions,
    progress: vscode.Progress<vscode.LanguageModelResponsePart>,
    token: vscode.CancellationToken,
  ): Promise<void> {
    const startedAt = Date.now();
    const input = languageModelMessagesToPrompt(messages);
    this.telemetry.event("lm.request.start", {
      modelId: model.id,
      provider: model.provider,
      messageCount: messages.length,
      inputChars: input.length,
    });
    const result = await this.turns.run(
      {
        input,
        token,
        reuseExistingSession: false,
        createSessionName: "vscode-lm",
        vars: {
          model: model.modelName,
          ...(model.provider ? { provider: model.provider } : {}),
        },
      },
      {
        onDelta: (event) => {
          if (event.content) {
            progress.report(new vscode.LanguageModelTextPart(event.content));
          }
        },
        onCompletedWithoutDelta: (_event, content) => {
          if (content) {
            progress.report(new vscode.LanguageModelTextPart(content));
          }
        },
        onPermissionRequested: async (_client, event) => {
          this.telemetry.warn("lm.approval.auto_denied", {
            approvalId: event.toolCall.toolCallId,
            toolName: event.toolCall._meta?.toolName,
            title: event.toolCall.title,
          });
          return { outcome: { outcome: "cancelled" } };
        },
      },
    );
    this.telemetry.event("lm.request.end", {
      modelId: model.id,
      failed: result.failed,
      durationMs: Date.now() - startedAt,
      sessionId: result.event.sessionId,
      turnId: result.event.turnId,
      stopReason: result.event.stopReason,
    });
    if (result.failed) {
      throw new Error(result.event.error || "Contenox language model request failed");
    }
  }

  public async provideTokenCount(
    _model: ContenoxLanguageModel,
    text: string | vscode.LanguageModelChatRequestMessage,
    _token: vscode.CancellationToken,
  ): Promise<number> {
    const value = typeof text === "string" ? text : languageModelMessagesToPrompt([text]);
    return estimateTokens(value);
  }
}

function modelInfoToLanguageModel(model: ModelInfo): ContenoxLanguageModel {
  const provider = model.provider?.trim() || undefined;
  const modelName = model.name || model.id;
  return {
    id: modelId(provider, modelName),
    name: provider ? `${model.displayName || modelName} (${provider})` : model.displayName || modelName,
    family: provider || "contenox",
    provider,
    modelName,
    version: model.id || modelName,
    maxInputTokens: model.contextLength && model.contextLength > 0 ? model.contextLength : 128000,
    maxOutputTokens: 8192,
    tooltip: provider ? `${provider}/${modelName}` : modelName,
    detail: model.source,
    isUserSelectable: true,
    capabilities: {
      toolCalling: true,
      imageInput: false,
    },
  };
}

function fallbackModel(provider: string | undefined, model: string | undefined): ContenoxLanguageModel {
  const modelName = model?.trim() || "default";
  const cleanProvider = provider?.trim() || undefined;
  return {
    id: modelId(cleanProvider, modelName),
    name: cleanProvider ? `${modelName} (${cleanProvider})` : "Contenox Default",
    family: cleanProvider || "contenox",
    provider: cleanProvider,
    modelName,
    version: modelName,
    maxInputTokens: 128000,
    maxOutputTokens: 8192,
    tooltip: "Contenox local runtime model",
    detail: "default",
    isUserSelectable: true,
    isDefault: true,
    capabilities: {
      toolCalling: true,
      imageInput: false,
    },
  };
}

function dedupeModels(models: readonly ContenoxLanguageModel[]): ContenoxLanguageModel[] {
  const seen = new Set<string>();
  const out: ContenoxLanguageModel[] = [];
  for (const model of models) {
    if (seen.has(model.id)) {
      continue;
    }
    seen.add(model.id);
    out.push(model);
  }
  return out;
}

// markDefaultModel flags the model matching the runtime's configured default
// as isDefault so VS Code can resolve a language model when the user has not
// explicitly picked one. Falls back to the first model so the picker always has
// a default and chat never fails with "Language model unavailable".
function markDefaultModel(
  models: ContenoxLanguageModel[],
  defaultProvider: string | undefined,
  defaultModel: string | undefined,
): void {
  if (models.length === 0) {
    return;
  }
  for (const model of models) {
    model.isDefault = false;
  }
  const wantProvider = defaultProvider?.trim() || undefined;
  const wantModel = defaultModel?.trim() || undefined;
  // Prefer an exact provider+name match, then name-only (the runtime's provider
  // label does not always equal the model's provider field), then the first
  // model so there is always a default.
  const match = wantModel
    ? models.find((m) => m.modelName === wantModel && wantProvider && m.provider === wantProvider) ??
      models.find((m) => m.modelName === wantModel)
    : undefined;
  (match ?? models[0]).isDefault = true;
}

function modelId(provider: string | undefined, model: string): string {
  return `${provider || "default"}:${model}`.replace(/[^A-Za-z0-9_.:-]/g, "_");
}

function languageModelMessagesToPrompt(messages: readonly vscode.LanguageModelChatRequestMessage[]): string {
  return messages
    .map((message) => `${roleLabel(message.role)}:\n${messagePartsToText(message.content)}`)
    .filter((part) => part.trim())
    .join("\n\n");
}

function roleLabel(role: vscode.LanguageModelChatMessageRole): string {
  switch (role) {
    case vscode.LanguageModelChatMessageRole.Assistant:
      return "Assistant";
    case vscode.LanguageModelChatMessageRole.User:
    default:
      return "User";
  }
}

function messagePartsToText(parts: readonly (vscode.LanguageModelInputPart | unknown)[]): string {
  return parts
    .map((part) => {
      if (part instanceof vscode.LanguageModelTextPart) {
        return part.value;
      }
      if (part instanceof vscode.LanguageModelToolResultPart) {
        return part.content.map((item) => (item instanceof vscode.LanguageModelTextPart ? item.value : "")).join("\n");
      }
      return "";
    })
    .filter(Boolean)
    .join("\n");
}

function estimateTokens(value: string): number {
  return Math.max(1, Math.ceil(value.length / 4));
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
