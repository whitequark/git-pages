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
        "github.com/ss098/certmagic-s3@v0.0.0-20250808023250-9788b7231c87"
      ];

      hash = "sha256-jZer6cBnE2Vo5/kMG+1vZBwWY8P/V1Lb33TA3Suz4pI=";
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
    cp ${self}/conf/Caddyfile /app/Caddyfile
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
