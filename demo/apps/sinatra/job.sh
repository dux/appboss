#!/bin/sh
# Prints to stdout and appends to log/job.log, which dboss tails into the log store as a file source.
mkdir -p log
while true; do
  line=$(awk 'BEGIN { srand(); for (i = 0; i < 5; i++) printf "%d ", 1 + int(rand() * 999); print "" }')
  echo "$line"
  echo "$(date -u +%FT%TZ) numbers $line" >> log/job.log
  sleep 3
done
