{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  buildInputs = with pkgs; [
    go
    gopls
    gotools
    go-tools
  ];

  shellHook = ''
    export GOPATH="$HOME/.go"
    export PATH="$GOPATH/bin:$PATH"
    export GO111MODULE=on
  '';
} 
