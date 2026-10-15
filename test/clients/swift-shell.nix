# Swift toolchain with Foundation for building iOS apps' Swift packages on Linux (test/clients/run-*.sh).
let p = (builtins.getFlake "nixpkgs").legacyPackages.${builtins.currentSystem};
in (p.mkShell.override { stdenv = p.swiftPackages.stdenv; }) {
  packages = with p.swiftPackages; [ swift Foundation Dispatch ];
}
