#!/bin/bash

NDK_PATH=~/Android/Sdk/ndk/27.0.12077973

env CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC=$NDK_PATH/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android21-clang go build -ldflags "-checklinkname=0" -o binaries/libmediamtx.so  .