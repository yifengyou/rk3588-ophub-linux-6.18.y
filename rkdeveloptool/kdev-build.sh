#!/bin/bash
set -x

CXX=aarch64-linux-gnu-g++
CXXFLAGS="-Wall -Wextra -Wreturn-type -fno-strict-aliasing -Os \
          -ffunction-sections -fdata-sections -fno-exceptions -fno-rtti \
          -D_FILE_OFFSET_BITS=64 -D_LARGE_FILE"
LDFLAGS="-static -Wl,--gc-sections -Wl,--strip-all"

# 编译：引用 $CXXFLAGS
$CXX $CXXFLAGS -c -o RKLocalTool.o RKLocalTool.cpp

# 链接：引用 $CXXFLAGS 和 $LDFLAGS
# 注意：链接阶段也需要传 CXXFLAGS，因为 g++ 需要它来选择合适的运行时
$CXX $CXXFLAGS $LDFLAGS -o rkdeveloptool RKLocalTool.o

ls -alh rkdeveloptool
