#!/bin/sh
set -eu
python3 -m unittest discover -s model/tests -v
sh verification/run_checks.sh
