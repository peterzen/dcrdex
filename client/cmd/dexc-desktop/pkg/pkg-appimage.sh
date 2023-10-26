#!/usr/bin/env bash

# This scipt uses pkg2appimage to build an AppImage for dexc-desktop.
# https://github.com/AppImageCommunity/pkg2appimage/blob/master/pkg2appimage


set -e

# turn this on for debugging, keep noise down for prod builds
# set -x

source $(dirname "$0")/common.sh

BUILD_DIR=./build
PKG2APPIMAGE_YML=pkg2appimage.yml

cd $BUILD_DIR

cat > $PKG2APPIMAGE_YML <<EOF
app: dexc

ingredients:
  dist: jammy
  sources:
    - deb http://archive.ubuntu.com/ubuntu/ jammy main universe
  debs:
    - $(pwd)/$DEB_NAME.deb
EOF

../pkg/pkg2appimage.sh pkg2appimage.yml >/dev/null

rm -f $PKG2APPIMAGE_YML
mv out/*.AppImage .

cd ..

ls $BUILD_DIR/*.AppImage
