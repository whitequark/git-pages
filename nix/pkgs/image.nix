{
  caddy,
  callPackage,
  dockerTools,
  git-pages,
  pkgsStatic,
  runtimeShell,
  self,
  upx,
  writeTextDir,
  ...
}:

let
  caddy' =
    (caddy.withPlugins {
      plugins = [
        "github.com/ss098/certmagic-s3=github.com/whitequark/certmagic-s3@v0.0.0-20250919212902-21ac26c15951"
      ];

      hash = "sha256-Mx+jSvjNt6y7mK4aP/YERmCPWx5UTGTyH2dbXUJ9+UY=";
    }).overrideAttrs
      (oldAttrs: {
        buildInputs = with pkgsStatic; [
          musl
        ];

        ldflags = oldAttrs.ldflags ++ [
          "-linkmode external"
          "-extldflags -static"
          "-s -w"
        ];
      });

  supervisord = callPackage ./supervisord.nix { };
in
dockerTools.buildImage {
  name = "git-pages";
  tag = "latest";

  copyToRoot = with dockerTools; [
    caCertificates
  ];

  runAsRoot = ''
    #!${runtimeShell}

    mkdir -p /app/data
    mkdir /bin

    cp ${self}/config.toml.example /app/config.toml
    ${caddy'}/bin/caddy adapt --config ${self}/conf/Caddyfile >/app/caddy.json
    cp ${self}/conf/supervisord.conf /app/supervisord.conf

    cp ${caddy'}/bin/caddy /bin/caddy
    cp ${git-pages}/bin/git-pages /bin/git-pages
    cp ${supervisord}/bin/supervisord /bin/supervisord

    chmod +w /bin/*
    ${upx}/bin/upx /bin/*
  '';

  config = {
    Cmd = [ "/bin/git-pages" ];
    WorkingDir = "/app";
  };
}
