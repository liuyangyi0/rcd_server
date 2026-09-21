#!/usr/bin/env bash
set -u

base_dir=/home/lenovo/code/rcd_manager_test
result_dir=$base_dir/soak-review5
soak_pid_file=$result_dir/soak-24h.pid
telemetry=$result_dir/telemetry-24h.log
summary=$result_dir/summary-24h.txt
manager_pid_file=$base_dir/manager.pid

snapshot() {
  now=$(date -Is)
  manager_pid=$(cat "$manager_pid_file" 2>/dev/null || true)
  rust_pid=$(pgrep -xo rcd_server 2>/dev/null || true)
  {
    echo "===== $now ====="
    echo "manager_process"
    if [ -n "$manager_pid" ]; then
      ps -p "$manager_pid" -o pid=,etime=,%cpu=,%mem=,rss=,vsz=,nlwp=,cmd=
    else
      echo missing
    fi
    echo "rust_process"
    if [ -n "$rust_pid" ]; then
      ps -p "$rust_pid" -o pid=,etime=,%cpu=,%mem=,rss=,vsz=,nlwp=,cmd=
    else
      echo missing
    fi
    echo "health"
    curl -fsS --max-time 10 http://127.0.0.1:8081/healthz || echo CURL_FAILED
    echo
    echo "ready"
    curl -fsS --max-time 10 http://127.0.0.1:8081/readyz || echo CURL_FAILED
    echo
    echo "system"
    curl -fsS --max-time 10 http://127.0.0.1:8081/api/v1/system || echo CURL_FAILED
    echo
    echo "overview"
    curl -fsS --max-time 10 http://127.0.0.1:8081/api/v1/overview || echo CURL_FAILED
    echo
    echo "sources"
    curl -fsS --max-time 10 http://127.0.0.1:8081/api/v1/sources || echo CURL_FAILED
    echo
    echo "sessions"
    curl -fsS --max-time 10 http://127.0.0.1:8081/api/v1/sessions || echo CURL_FAILED
    echo
    echo "pending"
    curl -fsS --max-time 10 'http://127.0.0.1:8081/api/v1/commands?status=Waiting&limit=100' || echo CURL_FAILED
    echo
    echo "manager_log_error_count=$(grep -Eic 'panic|fatal|data race|database is locked' "$base_dir/manager-review5-console.log" 2>/dev/null || true)"
    echo "rust_log_size=$(stat -c %s /tmp/rcd_console.log 2>/dev/null || echo 0)"
  } >> "$telemetry" 2>&1
}

mkdir -p "$result_dir"
: > "$telemetry"
snapshot
while true; do
  soak_pid=$(cat "$soak_pid_file" 2>/dev/null || true)
  if [ -z "$soak_pid" ] || ! kill -0 "$soak_pid" 2>/dev/null; then
    break
  fi
  sleep 60
  snapshot
done
sleep 5
snapshot

{
  echo "RCD Manager review-5 24h soak summary"
  echo "generated_at=$(date -Is)"
  echo "started_at=$(cat "$result_dir/soak-24h.started" 2>/dev/null || echo missing)"
  echo "finished_at=$(cat "$result_dir/soak-24h.finished" 2>/dev/null || echo still-running-or-interrupted)"
  echo "exit_code=$(cat "$result_dir/soak-24h.exit" 2>/dev/null || echo missing)"
  echo "telemetry_snapshots=$(grep -c '^===== ' "$telemetry" 2>/dev/null || echo 0)"
  echo "health_failures=$(grep -c 'CURL_FAILED' "$telemetry" 2>/dev/null || echo 0)"
  echo "manager_error_matches=$(grep -Eic 'panic|fatal|data race|database is locked' "$base_dir/manager-review5-console.log" 2>/dev/null || true)"
  echo "soak_result_lines"
  grep -E 'SOAK PASS|FAIL|PASS: TestLiveManagerSoak|subscription errors|ROUTE_REFRESH' "$result_dir/soak-24h.log" 2>/dev/null || true
  echo "soak_log_tail"
  tail -40 "$result_dir/soak-24h.log" 2>/dev/null || true
} > "$summary" 2>&1
