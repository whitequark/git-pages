{
  buildEnv,
  caddy,
  callPackage,
  dockerTools,
  git-pages,
  runtimeShell,
  self,
  writeTextDir,
  ...
}:

let
  caddy' = caddy.withPlugins {
    plugins = [
      "github.com/ss098/certmagic-s3@v0.0.0-20250808023250-9788b7231c87"
    ];

    hash = "sha256-jZer6cBnE2Vo5/kMG+1vZBwWY8P/V1Lb33TA3Suz4pI=";
  };

  supervisord = callPackage ./supervisord.nix { };

  supervisord-config = writeTextDir "app/supervisord.conf" ''
    [program-default]
    stderr_logfile = /dev/stderr
    stopsignal = TERM
    autorestart = true

    [program:pages]
    command = /bin/git-pages

    [program:caddy]
    command = /bin/caddy run
    depends_on = pages
  '';
in
dockerTools.buildImage {
  name = "git-pages";
  tag = "latest";

  copyToRoot = buildEnv {
    name = "image-root";

    paths = [
      caddy'
      git-pages
      supervisord
      supervisord-config

      dockerTools.caCertificates
    ];

    pathsToLink = [
      "/app"
      "/bin"
      "/etc"
    ];
  };

  runAsRoot = ''
    #!${runtimeShell}

    cp ${self}/Caddyfile /app/Caddyfile
    cp ${self}/config.toml.example /app/config.toml
    mkdir /app/data
  '';

  config = {
    Cmd = [ "/bin/git-pages" ];
    WorkingDir = "/app";
  };
}
