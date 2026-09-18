#!/bin/sh
while true; do
  awk 'BEGIN { srand(); for (i = 0; i < 5; i++) printf "%d ", 1 + int(rand() * 999); print "" }'
  sleep 3
done
