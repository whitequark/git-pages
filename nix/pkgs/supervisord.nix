{
  buildGoModule,
  fetchFromGitHub,
  fetchpatch,
  lib,
  pkgsStatic,
  ...
}:

buildGoModule rec {
  pname = "supervisord";
  version = "0.7.3";

  src = fetchFromGitHub {
    owner = "ochinchina";
    repo = pname;
    rev = "16cb640325b3a4962b2ba17d68fb5c2b1e1b6b3c";
    hash = "sha256-NPlU2f+zXw1qHWKTyTghQmulDuphpLZ3K/Pr/K9J7KI=";
  };

  buildInputs = with pkgsStatic; [
    musl
  ];

  tags = [
    "release"
  ];

  ldflags = [
    "-linkmode external"
    "-extldflags -static"
    "-s -w"
  ];

  subPackages = ".";

  vendorHash = "sha256-W/68Kq5Z9+7fUKQGq1/hI12pLznlKRYw7x464ZJVxtM=";

  preBuild = ''
    go generate -tags ${lib.concatStringsSep "," tags}
  '';
}
