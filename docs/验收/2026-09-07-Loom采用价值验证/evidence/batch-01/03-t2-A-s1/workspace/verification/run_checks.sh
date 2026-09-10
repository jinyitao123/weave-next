#!/bin/sh
set -eu
cd model
timeout 120 python3 -m unittest discover -s tests -v
