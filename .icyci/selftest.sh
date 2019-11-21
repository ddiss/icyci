# SPDX-License-Identifier: AGPL-3.0-only
#
# Copyright (C) 2019 SUSE LLC

# This test script is invoked by icyCI to test itself, e.g.:
# - dev pushes signed commit / tag to icyci repo
# - host running (current) icyci detects change, validates signature and
#   executes this script in the checked out source

go test || exit 1

# TODO install and restart icyCI service if test passed
