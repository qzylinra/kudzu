{
  lib,
  mcp-nixos,
  context7-mcp,
  uv,
  writeText,
  formats,
}:

(formats.toml { }).generate "config.toml" {
  model_provider = "zen";
  model = "deepseek-v4-flash-free";
  model_reasoning_effort = "xhigh";
  model_catalog_json = "~/.openinterpreter/zen_model_catalog.json";
  sandbox_mode = "danger-full-access";
  approval_policy = "never";
  approvals_reviewer = "auto_review";
  harness_guidance = false;
  check_for_update_on_startup = false;
  sandbox_workspace_write = {
    network_access = true;
  };
  history = {
    persistence = "save-all";
  };
  model_providers = {
    zen = {
      name = "zen";
      base_url = "http://127.0.0.1:8787/v1";
      api_key = "sk-zen";
      wire_api = "chat";
      request_max_retries = 7;
      stream_max_retries = 9;
    };
  };
  mcp_servers = {
    context7-mcp = {
      command = lib.getExe context7-mcp;
      default_tools_approval_mode = "approve";
    };
    mcp-nixos = {
      command = lib.getExe mcp-nixos;
      default_tools_approval_mode = "approve";
    };
    android-mcp = {
      command = lib.getExe' uv "uvx";
      args = [
        "--python"
        "3.14"
        "android-mcp"
      ];
      default_tools_approval_mode = "approve";
    };
    open-websearch = {
      command = "open-websearch";
      env = {
        SEARCH_MODE = "request";
        DEFAULT_SEARCH_ENGINE = "duckduckgo";
      };
      default_tools_approval_mode = "approve";
    };
  };
  shell_environment_policy = {
    "inherit" = "all";
    ignore_default_excludes = false;
  };
  agents = {
    max_threads = 4;
    max_depth = 1;
    job_max_runtime_seconds = 9000;
  };
}
