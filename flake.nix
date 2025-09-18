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
          version = "1.0.0";

          src = nix-filter {
            root = self;

            include = [
              "go.mod"
              "go.sum"

              (nix-filter.lib.inDirectory "src")
            ];
          };

          vendorHash = "sha256-WVnxNtCCk6T+EsT6Wvd+yR2mxU03SNnSwpeYlYLOCGU=";

          fixupPhase = ''
            # Apparently `go install` doesn't support renaming the binary, so country girls make do.
            mv $out/bin/{src,git-pages}
          '';
        };
      in
      {
        formatter = pkgs.nixfmt-tree;

        devShells.default = pkgs.mkShell {
          inputsFrom = [
            git-pages
          ];
        };

        packages.default = git-pages;
      }
    );
}
