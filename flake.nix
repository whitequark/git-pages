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

          vendorHash = "sha256-qmBccvKmXjWvycvUfPkzy5Q/TZ7BT946ZFq+11w7gpc=";

          fixupPhase = ''
            # Apparently `go install` doesn't support renaming the binary, so country girls make do.
            mv $out/bin/{src,git-pages}
          '';
        };

        image = pkgs.callPackage ./nix/pkgs/image.nix { inherit git-pages self; };
      in
      {
        formatter = pkgs.nixfmt-tree;

        devShells.default = pkgs.mkShell {
          inputsFrom = [
            git-pages
          ];
        };

        packages = {
          inherit git-pages image;
          default = git-pages;
        };
      }
    );
}
