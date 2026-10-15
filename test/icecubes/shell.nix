# Swift toolchain with Foundation for building Ice Cubes' packages on Linux.
let p = (builtins.getFlake "nixpkgs").legacyPackages.${builtins.currentSystem};
in (p.mkShell.override { stdenv = p.swiftPackages.stdenv; }) {
  packages = with p.swiftPackages; [ swift Foundation Dispatch ];
}
