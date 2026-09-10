#!/bin/sh
set -eu
cd "$(dirname "$0")/../model"
PYTHONPATH=.. python3 -m unittest discover -s tests -v
