#!/bin/sh
# Prepares cgroup v2 so nsjail can give every execution its own memory / pids /
# cpu limits, then starts the server.
#
# cgroup v2 forbids enabling controllers for children of a cgroup that itself
# contains processes, so we first move ourselves into a leaf ("init") and then
# delegate the controllers from the container's root cgroup.
set -eu

CG=/sys/fs/cgroup
if [ -f "$CG/cgroup.controllers" ]; then
  if [ ! -w "$CG/cgroup.subtree_control" ]; then
    mount -o remount,rw "$CG" 2>/dev/null || true
  fi
  if [ -w "$CG/cgroup.subtree_control" ]; then
    mkdir -p "$CG/init"
    for pid in $(cat "$CG/cgroup.procs"); do
      echo "$pid" > "$CG/init/cgroup.procs" 2>/dev/null || true
    done
    for c in memory pids cpu; do
      echo "+$c" > "$CG/cgroup.subtree_control" 2>/dev/null || echo "entrypoint: cannot enable cgroup controller $c" >&2
    done
  else
    echo "entrypoint: cgroup v2 not writable; sandbox falls back to rlimits (see README)" >&2
  fi
fi

# Docker overmounts parts of /proc (kcore, keys, ...). The kernel refuses to
# mount procfs inside a new user namespace unless a fully visible procfs
# exists, so expose one read-only in a root-only directory. Jailed code never
# sees it; it only satisfies the kernel's check so each jail gets its own /proc.
if [ ! -d /run/sandbox/proc/self ]; then
  mkdir -p -m 0700 /run/sandbox /run/sandbox/proc
  mount -t proc -o ro proc /run/sandbox/proc 2>/dev/null \
    || echo "entrypoint: could not mount clean procfs; nsjail will fail to mount /proc" >&2
fi

exec "$@"
