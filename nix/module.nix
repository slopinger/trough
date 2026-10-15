{ self }: { config, lib, pkgs, ... }: let
  cfg = config.services.trough;
in {
  options.services.trough = {
    enable = lib.mkEnableOption "trough ActivityPub server";
    package = lib.mkOption { type = lib.types.package; default = self.packages.${pkgs.stdenv.hostPlatform.system}.default; description = "trough executable package."; };
    host = lib.mkOption { type = lib.types.str; description = "Permanent public instance hostname (embedded in federation IDs)."; };
    listen = lib.mkOption { type = lib.types.str; default = "127.0.0.1:8790"; description = "HTTP listen address; use a reverse proxy for TLS."; };
    environmentFile = lib.mkOption { type = lib.types.nullOr lib.types.str; default = null; description = "Optional runtime environment file for secrets; never a Nix store path."; };
  };
  config = lib.mkIf cfg.enable {
    users.users.trough = { isSystemUser = true; group = "trough"; home = "/var/lib/trough"; };
    users.groups.trough = {};
    systemd.services.trough = {
      description = "trough ActivityPub server";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      environment = { TROUGH_HOST = cfg.host; TROUGH_LISTEN = cfg.listen; TROUGH_DATA = "/var/lib/trough"; };
      serviceConfig = {
        User = "trough";
        Group = "trough";
        ExecStart = "${lib.getExe cfg.package} serve";
        EnvironmentFile = lib.optional (cfg.environmentFile != null) cfg.environmentFile;
        Restart = "on-failure";
        RestartSec = 2;
        StateDirectory = "trough";
        StateDirectoryMode = "0750";
        UMask = "0027";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        CapabilityBoundingSet = "";
      };
    };
    environment.systemPackages = [ cfg.package ];
  };
}
