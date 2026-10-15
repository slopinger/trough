{
  description = "trough ActivityPub server";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  outputs = { self, nixpkgs }: let
    systems = [ "x86_64-linux" "aarch64-linux" ];
    eachSystem = nixpkgs.lib.genAttrs systems;
  in {
    packages = eachSystem (system: let pkgs = nixpkgs.legacyPackages.${system}; in {
      default = pkgs.buildGo127Module {
        pname = "trough";
        version = "0.1.0";
        src = self;
        vendorHash = "sha256-DeaWjQtYLjAkrq1MJz7TBzUDza4emoS502RsS42AXGE=";
        env.CGO_ENABLED = 0;
        ldflags = [ "-s" "-w" ];
        meta = {
          description = "SQLite ActivityPub server with a server-rendered web UI";
          license = pkgs.lib.licenses.mit;
          mainProgram = "trough";
          platforms = systems;
        };
      };
    });
    checks = eachSystem (system: { package = self.packages.${system}.default; });
    devShells = eachSystem (system: {
      default = nixpkgs.legacyPackages.${system}.mkShell {
        packages = with nixpkgs.legacyPackages.${system}; [ go_1_27 python3 caddy sqlite ];
      };
    });
    nixosModules.default = import ./nix/module.nix { inherit self; };
  };
}
