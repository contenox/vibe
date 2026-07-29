import * as vscode from "vscode";
import { BridgeProcess } from "../bridge/BridgeProcess";
import { ModelInfo, ProviderInfo } from "../bridge/protocol";

export async function selectProvider(bridge: BridgeProcess): Promise<string | undefined> {
  const client = await requireClient(bridge);
  const result = await client.listProviders();
  const selected = await vscode.window.showQuickPick(result.providers.map(providerPick), {
    title: "Select Contenox Provider",
    placeHolder: "Provider",
  });
  if (!selected) {
    return undefined;
  }

  await client.setConfig({ defaultProvider: selected.provider.provider });
  await bridge.refreshHealth();
  vscode.window.showInformationMessage(`Contenox provider set to ${selected.provider.provider}`);
  return selected.provider.provider;
}

export async function selectChatModel(bridge: BridgeProcess): Promise<string | undefined> {
  const client = await requireClient(bridge);
  const config = await client.getConfig();
  const provider = config.defaultProvider;
  const result = await client.listModels(provider ? { provider } : undefined);
  const selected = await vscode.window.showQuickPick(result.models.map(modelPick), {
    title: "Select Contenox Chat Model",
    placeHolder: provider ? `Model for ${provider}` : "Model",
  });
  if (!selected) {
    return undefined;
  }

  await client.setConfig({ defaultModel: selected.model.name });
  await bridge.refreshHealth();
  vscode.window.showInformationMessage(`Contenox chat model set to ${selected.model.displayName}`);
  return selected.model.name;
}

export async function selectAutocompleteProvider(bridge: BridgeProcess): Promise<string | undefined> {
  const client = await requireClient(bridge);
  const [config, result] = await Promise.all([client.getConfig(), client.listProviders()]);
  const current = config.defaultAutocompleteProvider ?? "";
  const picks = [
    {
      label: "Use runtime default",
      description: current ? undefined : "active",
      provider: "",
    },
    ...result.providers.map((provider) => ({
      ...providerPick(provider),
      description: provider.provider === current ? "active" : provider.configured ? "configured" : "available",
    })),
  ];
  const selected = await vscode.window.showQuickPick(picks, {
    title: "Select Contenox Autocomplete Provider",
    placeHolder: "Autocomplete provider",
  });
  if (!selected) {
    return undefined;
  }

  const provider = typeof selected.provider === "string" ? selected.provider : selected.provider.provider;
  await client.setConfig({ defaultAutocompleteProvider: provider });
  await bridge.refreshHealth();
  vscode.window.showInformationMessage(provider ? `Contenox autocomplete provider set to ${provider}` : "Contenox autocomplete uses the runtime default");
  return provider;
}

export async function selectAutocompleteModel(bridge: BridgeProcess): Promise<string | undefined> {
  const client = await requireClient(bridge);
  const bridgeConfig = await client.getConfig();
  const provider = bridgeConfig.defaultAutocompleteProvider || bridgeConfig.defaultProvider;
  const current = bridgeConfig.defaultAutocompleteModel ?? "";
  const result = await client.listModels(provider ? { provider } : undefined);
  const picks = [
    {
      label: "Use runtime default",
      description: current ? undefined : "active",
      model: undefined as ModelInfo | undefined,
    },
    ...result.models.map((model) => ({
      ...modelPick(model),
      description: model.name === current ? "active" : model.provider || model.source,
    })),
  ];
  const selected = await vscode.window.showQuickPick(picks, {
    title: "Select Contenox Autocomplete Model",
    placeHolder: provider ? `Autocomplete model for ${provider}` : "Autocomplete model",
  });
  if (!selected) {
    return undefined;
  }

  const model = selected.model?.name ?? "";
  await client.setConfig({ defaultAutocompleteModel: model });
  await bridge.refreshHealth();
  vscode.window.showInformationMessage(model ? `Contenox autocomplete model set to ${selected.model?.displayName ?? model}` : "Contenox autocomplete uses the runtime default");
  return model;
}

export async function selectHitlPolicy(bridge: BridgeProcess): Promise<string | undefined> {
  const client = await requireClient(bridge);
  const [config, policies] = await Promise.all([client.getConfig(), client.listHitlPolicies()]);
  const selected = await vscode.window.showQuickPick(
    policies.policies.map((policy) => ({
      label: policy,
      description: policy === config.hitlPolicyName ? "active" : undefined,
      policy,
    })),
    {
      title: "Select Contenox HITL Policy",
      placeHolder: "HITL policy",
    },
  );
  if (!selected) {
    return undefined;
  }

  await client.setConfig({ hitlPolicyName: selected.policy });
  await bridge.refreshHealth();
  vscode.window.showInformationMessage(`Contenox HITL policy set to ${selected.policy}`);
  return selected.policy;
}

export async function selectThinkLevel(bridge: BridgeProcess): Promise<string | undefined> {
  const client = await requireClient(bridge);
  const config = await client.getConfig();
  const levels = ["auto", "off", "minimal", "low", "medium", "high", "xhigh"];
  const selected = await vscode.window.showQuickPick(
    levels.map((level) => ({
      label: level,
      description: level === config.defaultThink ? "active" : undefined,
      level,
    })),
    {
      title: "Select Contenox Thinking Level",
      placeHolder: "Reasoning level",
    },
  );
  if (!selected) {
    return undefined;
  }

  await client.setConfig({ defaultThink: selected.level });
  await bridge.refreshHealth();
  vscode.window.showInformationMessage(`Contenox thinking level set to ${selected.level}`);
  return selected.level;
}

async function requireClient(bridge: BridgeProcess) {
  await bridge.ensureStarted();
  const client = bridge.currentClient;
  if (!client) {
    throw new Error("Contenox runtime connection is not available");
  }
  return client;
}

function providerPick(provider: ProviderInfo): vscode.QuickPickItem & { provider: ProviderInfo } {
  const configured = provider.configured ? "configured" : "available";
  const detail = provider.baseUrl || provider.recommendedApiKeyEnv || "";
  return {
    label: provider.provider,
    description: configured,
    detail,
    provider,
  };
}

function modelPick(model: ModelInfo): vscode.QuickPickItem & { model: ModelInfo } {
  return {
    label: model.displayName,
    description: model.provider || model.source,
    detail: model.contextLength ? `Context ${model.contextLength}` : undefined,
    model,
  };
}
