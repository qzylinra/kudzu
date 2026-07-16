{
  lib,
  runCommand,
  makeWrapper,
  opencode,
  rust-analyzer,
  ktlint,
  nixd,
  ruff,
  bun,
  openspec,
}:

runCommand "opencode"
  {
    buildInputs = [ makeWrapper ];
  }
  ''
    mkdir --parents $out/bin
    makeWrapper ${lib.getExe opencode} $out/bin/opencode \
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
      --set OPENCODE_DISABLE_DEFAULT_PLUGINS true \
      --set OPENCODE_EXPERIMENTAL_PARALLEL true \
      --set OPENCODE_ENABLE_EXPERIMENTAL_MODELS true \
      --set OPENCODE_EXPERIMENTAL_LSP_TY true
  ''
