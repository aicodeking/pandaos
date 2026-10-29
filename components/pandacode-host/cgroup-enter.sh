# pandacode-host-enter CMD...: ExecStart wrapper for the delegated
# (Delegate=yes) pandacode-host cgroup. Moves the host into a leaf `host/`
# that oomd avoids, enables controllers for per-task cgroups (made by the
# agent CLI shims under $PANDA_TASK_CGROUP_ROOT), sweeps empty task cgroups,
# then execs CMD.
root=/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)
mkdir -p "$root/host"
echo "$$" >"$root/host/cgroup.procs"
# oomd: kill a task cgroup, never the host, under memory pressure.
setfattr -n user.oomd_avoid -v 1 "$root/host" ||
  echo "pandacode-host-enter: cannot mark $root/host oomd_avoid" >&2

# Only possible now that $root holds no processes (cgroup v2 rule).
want=
read -ra have <"$root/cgroup.controllers"
for c in "${have[@]}"; do
  case $c in memory | pids | cpu) want="$want +$c" ;; esac
done
if [ -n "$want" ]; then
  echo "${want# }" >"$root/cgroup.subtree_control"
fi

# A shim can't rmdir its own task cgroup after exec, so sweep empty ones.
# ponytail: 30 s polling; inotify on cgroup.events if it ever matters.
(
  while sleep 30; do
    for d in "$root"/task-*; do
      if [ -d "$d" ] && grep -qx 'populated 0' "$d/cgroup.events"; then
        rmdir "$d" 2>/dev/null || true
      fi
    done
  done
) &

export PANDA_TASK_CGROUP_ROOT=$root
exec "$@"
