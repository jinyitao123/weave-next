#!/bin/sh
set -eu
cd outputs/model
timeout 120 python3 -m unittest discover -s tests -v
