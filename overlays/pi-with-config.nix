{
  lib,
  runCommand,
  makeWrapper,
  pi-coding-agent,
  rust-analyzer,
  ktlint,
  nixd,
  ruff,
  bun,
  openspec,
  nixfmt,
  rustfmt,
  shfmt,
  taplo,
  yamlfmt,
  mcp-nixos,
  mcp-server-filesystem,
  mcp-server-git,
  mcp-server-fetch,
  github-mcp-server,
  context7-mcp,
  nodejs-slim,
  uv,
  writeText,
  kotlin-language-server,
  agent-browser,
}:

runCommand "pi"
  {
    buildInputs = [ makeWrapper ];
  }
  ''
    mkdir --parents $out/bin
    makeWrapper ${lib.getExe pi-coding-agent} $out/bin/pi \
      --prefix PATH : ${
        lib.makeBinPath [
          rust-analyzer
          ktlint
          nixd
          ruff
          bun
          openspec
        ]
      } \
      --add-flag "--approve"
  ''
