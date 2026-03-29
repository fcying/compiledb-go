#!/usr/bin/bash

set -eu

root=/opt/compiledb_test
src_dir="$root/src"

mkdir -p "$src_dir"
mkdir -p "$root/sub/nested"
mkdir -p "$root/build"
mkdir -p "$root/relative-build"
mkdir -p "$src_dir/src dir"

cd "$src_dir"

touch test1.c
touch test1.cc
touch test1.cpp
touch test_none.c

touch "$root/test2.c"
touch "$root/sub/nested/sub_file.c"
touch "$root/quoted-name.c"
touch "$root/relative-build/src3.cc"
touch "$src_dir/src dir/test space.c"
