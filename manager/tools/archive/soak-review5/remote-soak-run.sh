#!/usr/bin/env bash
set -u

base_dir=/home/lenovo/code/rcd_manager_test
source_dir=$base_dir/build-review5b/src
result_dir=$base_dir/soak-review5

mkdir -p "$result_dir"
cd "$source_dir" || exit 2
export PATH=$base_dir/tools/go/bin:$PATH
export GOPROXY=off
export RCD_MANAGER_LIVE_ENDPOINT=opc.tcp://127.0.0.1:4842
export RCD_MANAGER_LIVE_GROUP=remote-line
export RCD_MANAGER_LIVE_SOURCE_A=rcd-46-none
export RCD_MANAGER_LIVE_SOURCE_B=rcd-46-secure-anon
export RCD_MANAGER_LIVE_SOAK_DURATION=24h
export RCD_MANAGER_LIVE_CLIENTS=10
export RCD_MANAGER_LIVE_SWITCH_INTERVAL=15m

date -Is > "$result_dir/soak-24h.started"
go test ./internal/opc -run '^TestLiveManagerSoak$' -count=1 -timeout=25h -v \
  > "$result_dir/soak-24h.log" 2>&1
test_rc=$?
printf '%s\n' "$test_rc" > "$result_dir/soak-24h.exit"
date -Is > "$result_dir/soak-24h.finished"
exit "$test_rc"
