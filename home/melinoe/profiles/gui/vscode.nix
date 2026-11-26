{ pkgs, ... }:
{
  programs.vscodium = {
    enable = true;
    package = pkgs.vscodium.overrideAttrs (old: {
      postPatch = (old.postPatch or "") + ''
        jq '.dataFolderName = ".local/share/vscode-oss" | .sharedDataFolderName = ".local/share/vscode-oss-shared"' \
          resources/app/product.json > product.json.tmp
        mv product.json.tmp resources/app/product.json
      '';
    });
  };
}
