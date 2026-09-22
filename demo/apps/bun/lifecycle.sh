#!/bin/sh
# Stand-in for an app-owned database: create makes it, start checks it, destroy drops it.
db=.dboss/demo.db
case "$1" in
  create) mkdir -p .dboss && date -u +%FT%TZ > "$db" && echo "created $db" ;;
  start) test -f "$db" && echo "start: $db from $(cat "$db")" || { echo "start: $db missing" >&2; exit 1; } ;;
  destroy) rm -f "$db" && echo "dropped $db" ;;
esac
