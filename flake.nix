{
  description = "A minimal, self-hosted agent harness";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      revision = self.shortRev or self.dirtyShortRev or "unknown";
    in
    {
      packages = forAllSystems (pkgs: {
        default = pkgs.buildGoModule {
          pname = "noctiluca";
          version = revision;
          src = self;
          vendorHash = "sha256-5dSkC5zQssFDSR2ZvxSM2/avoGhk4LPrcnbEG53fnMY=";
          env.CGO_ENABLED = "0";
          subPackages = [ "cmd/noctiluca" ];
          ldflags = [ "-s" "-w" "-X main.version=${revision}" "-X main.revision=${revision}" ];
          nativeCheckInputs = [ pkgs.git ];
          checkPhase = ''
            runHook preCheck
            go test -shuffle=on ./...
            runHook postCheck
          '';
          meta = {
            description = "A minimal, self-hosted agent harness";
            license = pkgs.lib.licenses.mit;
            mainProgram = "noctiluca";
            platforms = systems;
          };
        };
      });

      checks = forAllSystems (pkgs: {
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.golangci-lint pkgs.mise pkgs.git ];
          CGO_ENABLED = "0";
        };
      });
    };
}
