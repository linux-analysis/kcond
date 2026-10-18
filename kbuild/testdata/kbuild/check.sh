#!/bin/sh
# Runs the minimized Makefiles of this directory through
# the Kbuild of a kernel release and prints how it builds each object in
# each configuration their header lists: the expectations of
# TestKbuildSemantics. Usage:
#   sh kbuild/testdata/kbuild/check.sh LINUX TAG > kbuild/testdata/kbuild/expected.txt
set -e
linux=$1
tag=$2
cases=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git -C "$linux" archive "$tag" scripts | tar -x -C "$work"
mkdir -p "$work/d" "$work/include/config"
cat > "$work/dump.mk" <<'EOF'
dump:
	@for o in $(notdir $(real-obj-y) $(lib-y)); do echo "$$o builtin"; done
	@for o in $(notdir $(real-obj-m)); do echo "$$o module"; done
EOF
for mk in "$cases"/*.mk; do
	name=$(basename "$mk" .mk)
	cp "$mk" "$work/d/Makefile"
	sed -n 's/^# configs: //p' "$mk" | tr ';' '\n' | while read -r cfg; do
		printf '%s\n' $cfg > "$work/include/config/auto.conf"
		make -s -C "$work" -f scripts/Makefile.build -f dump.mk obj=d srctree=. objtree=. srcroot=. \
			kbuild-file=d/Makefile need-builtin=1 need-modorder=1 dump |
			sort -u | while read -r obj state; do
				printf '%s\t%s\t%s\t%s\n' "$name" "$cfg" "$obj" "$state"
			done
	done
done
