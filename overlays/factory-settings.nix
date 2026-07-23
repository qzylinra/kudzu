{
  writeText,
}:

writeText "settings.json" (
  builtins.toJSON {
    cloudSessionSync = false;
    includeCoAuthoredByDroid = false;
    enableDroidShield = false;
    showThinkingInMainView = true;
    reasoningEffort = "high";
    sessionDefaultSettings.autonomyLevel = "high";
    completionSound = "off";
    awaitingInputSound = "off";
    soundFocusMode = "focused";
    model = "mimo-v2.5-free";
    diffMode = "unified";
    logoAnimation = "off";
    disableUsageLimitAlerts = true;
    disableWeeklyUsageSummary = true;
    wikiCloudSync = false;
    sandbox.enabled = false;
    missionModelSettings.workerModel = "deepseek-v4-flash-free";
    missionModelSettings.workerReasoningEffort = "high";
    subagentModelSettings.lightModel = "deepseek-v4-flash-free";
    subagentModelSettings.mediumModel = "deepseek-v4-flash-free";
    subagentModelSettings.heavyReasoningEffort = "high";
    subagentModelSettings.mediumReasoningEffort = "high";
    subagentModelSettings.lightReasoningEffort = "high";
    customModels = [
      {
        model = "mimo-v2.5-free";
        baseUrl = "http://127.0.0.1:8787/v1";
        apiKey = "x";
        provider = "generic-chat-completion-api";
      }
      {
        model = "deepseek-v4-flash-free";
        baseUrl = "http://127.0.0.1:8787/v1";
        apiKey = "x";
        provider = "generic-chat-completion-api";
      }
    ];
  }
)
