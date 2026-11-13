#!/usr/bin/env bash
# Rebuilds the fixture jars of the deps tests from the sources next to this
# script. Needs a JDK 17 or later; the tests only read the committed jars.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT
javac --release 17 -g -d "$out/lib" $(find src -name '*.java')
javac --release 17 -g:none -d "$out/lib" nodebug/com/example/lib/NoDebug.java
javac --release 17 -g -d "$out/helper" sources/com/example/helper/Helper.java
# Fixed timestamps keep the jars byte-identical across rebuilds.
find "$out" -exec touch -t 202601010000 {} +
(cd "$out/lib" && jar --create --file "$OLDPWD/lib-1.0.jar" --date=2026-01-01T00:00:00Z $(find . -name '*.class' | sort))
(cd "$out/helper" && jar --create --file "$OLDPWD/helper-2.0.jar" --date=2026-01-01T00:00:00Z $(find . -name '*.class' | sort))
(cd sources && jar --create --file "../helper-2.0-sources.jar" --date=2026-01-01T00:00:00Z $(find . -name '*.java' | sort))
