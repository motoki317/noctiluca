#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
scratch=$(mktemp -d "$PWD/.build.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
export PYTHONDONTWRITEBYTECODE=1

nix shell nixpkgs#blender nixpkgs#resvg --command bash -e -c '
    blender --background --factory-startup --python-exit-code 1 --python scene.py -- 640 "$1/scene-2x1.png"
    blender --background --factory-startup --python-exit-code 1 --python scene.py -- 720 "$1/scene-16x9.png"
    uv run --no-project --with numpy --with pillow --with pypng python compose.py "$1/scene-2x1.png" "$1/social-2x1.png"
    uv run --no-project --with numpy --with pillow --with pypng python compose.py "$1/scene-16x9.png" "$1/social-16x9.png"
' bash "$scratch"

mv -- "$scratch/social-2x1.png" "$scratch/social-16x9.png" ..
