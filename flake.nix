{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    nix-filter.url = "github:numtide/nix-filter";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      nix-filter,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        git-pages = pkgs.buildGo125Module {
          pname = "git-pages";
          version = "0";

          src = nix-filter {
            root = self;

            include = [
              "go.mod"
              "go.sum"
              "main.go"

              (nix-filter.lib.inDirectory "src")
            ];
          };

          buildInputs = with pkgs; [
            pkgsStatic.musl
          ];

          ldflags = [
            "-linkmode external"
            "-extldflags -static"
            "-s -w"
          ];

          vendorHash = "sha256-opS3f4GDczDRp7mrBzvQtK13Qi4snanX4I64FHTh7Pw=";
        };
      in
      {
        formatter = pkgs.nixfmt-tree;

        devShells.default = pkgs.mkShell {
          inputsFrom = [
            git-pages
          ];

          packages = with pkgs; [
            caddy
          ];
        };

        packages = {
          inherit git-pages;
          default = git-pages;
        };
      }
    );
}
