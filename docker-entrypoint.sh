#!/bin/sh
set -eu

data_dir=/data
mkdir -p "$data_dir/logs"

unowned_path=$(find "$data_dir" -xdev ! -user newapi -print -quit)
if [ -n "$unowned_path" ]; then
  find "$data_dir" -xdev ! -user newapi -exec chown newapi:newapi {} +
fi

exec gosu newapi "$@"
