{
  description = "j-cicd - self-hosted CI/CD pipeline runner";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;

      # Single source of truth for the version: it's stamped into the
      # binary below, so `j-cicd version` can't drift from what was built.
      version = "0.1.0";
    in
    {
      packages = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system};
        in {
          default = pkgs.buildGoModule {
            pname = "j-cicd";
            inherit version;
            src = ./.;
            vendorHash = "sha256-cpZVWNgH/SoTu117Iby4QExgP0ROPzWju6A0iUkyQ1o=";

            subPackages = [ "cmd/j-cicd" ];
            ldflags = [ "-X main.version=${version}" ];

            meta = with pkgs.lib; {
              description = "Self-hosted CI/CD pipeline runner";
              mainProgram = "j-cicd";
              license = licenses.mit;
            };
          };
        });

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/j-cicd";
        };
      });

      devShells = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system};
        in {
          default = pkgs.mkShell {
            packages = [ pkgs.go pkgs.docker-client ];
          };
        });
    };
}
