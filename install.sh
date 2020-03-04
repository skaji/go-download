#!/bin/bash

set -eu

if [[ $# -ne 1 ]] || [[ $1 = "-h" ]] || [[ $1 = "--help" ]]; then
  echo "Usage: install.sh INSTALL_DIR"
  exit 1
fi

INSTALL_DIR="$1"

LATEST_VERSION="$(curl -Is https://github.com/skaji/go-download/releases/latest | grep -i location: | tr -d '\r')"
LATEST_VERSION=${LATEST_VERSION##*/}

CURRENT_VERSION=v0.0.0
if [[ -f "$INSTALL_DIR/download" ]]; then
  CURRENT_VERSION=v$("$INSTALL_DIR/download" --version)
fi

if [[ $LATEST_VERSION = $CURRENT_VERSION ]]; then
  echo "You already have the latest version $LATEST_VERSION" >&2
  exit
fi

if [[ ! -d $INSTALL_DIR ]]; then
  if [[ -e $INSTALL_DIR ]]; then
    echo "$INSTALL_DIR is not a directory" >&2
    exit 1
  fi
  mkdir -p $INSTALL_DIR
  if [[ ! -d $INSTALL_DIR ]]; then
    echo "Failed to create $INSTALL_DIR" >&2
    exit 1
  fi
fi

if [[ ! $INSTALL_DIR =~ ^/ ]]; then
  INSTALL_DIR=$PWD/$INSTALL_DIR
fi

WORKDIR=$(mktemp -d)
trap "rt=\$?; cd /; rm -rf $WORKDIR; exit \$rt" EXIT INT TERM
cd $WORKDIR

DIRNAME=download-$(uname -s | tr '[A-Z]' '[a-z]')-amd64
URL=https://github.com/skaji/go-download/releases/download/$LATEST_VERSION/$DIRNAME.tar.gz
echo "Downloading $URL" >&2
curl -fsSL -o $DIRNAME.tar.gz $URL
tar xf $DIRNAME.tar.gz
mv -f $DIRNAME/download $INSTALL_DIR/
