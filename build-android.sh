#!/bin/bash

env CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC=~/Android/Sdk/ndk/21.4.7075529/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android21-clang ~/go1.24.3/go/bin/go build -ldflags "-checklinkname=0" -o binaries/libmediamtx.so  .