{ writeShellScriptBin }:

writeShellScriptBin "codex" ''
  exec interpreter --yolo $@
''
