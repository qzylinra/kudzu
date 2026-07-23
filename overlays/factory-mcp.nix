{
  lib,
  writeText,
  mcp-nixos,
  context7-mcp,
  uv,
  nodejs-slim,
}:

writeText "mcp.json" (
  builtins.toJSON {
    mcpServers = {
      mcp-nixos = {
        type = "stdio";
        command = lib.getExe mcp-nixos;
      };
      context7-mcp = {
        type = "stdio";
        command = lib.getExe context7-mcp;
      };
      android-mcp = {
        type = "stdio";
        command = lib.getExe' uv "uvx";
        args = [
          "--python"
          "3.13"
          "android-mcp"
        ];
      };
      open-websearch = {
        type = "stdio";
        command = "open-websearch";
        env = {
          SEARCH_MODE = "request";
          DEFAULT_SEARCH_ENGINE = "duckduckgo";
        };
      };
    };
  }
)
