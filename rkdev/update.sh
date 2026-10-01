#!/bin/bash

set -x

./kdev-build.sh
sshpass -p root ssh root@192.168.33.50 pkill rkdev_arm64
sshpass -p root scp rkdev_arm64 192.168.33.50:/root/

