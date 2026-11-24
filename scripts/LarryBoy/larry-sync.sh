#!/bin/sh
# Runs larry-sync the way grout's own launch script runs grout: from the pak
# folder, so it finds config.json and the catalog, with the firmware named and
# the bundled libraries on the path.
DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR" || exit 1

export CFW=NEXTUI
export LD_LIBRARY_PATH="$DIR/lib:$LD_LIBRARY_PATH"

exec ./larry-sync "$@"
