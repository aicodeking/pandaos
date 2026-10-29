# Agent CLI shim (module.nix prepends `name=<cli>`). First on the
# pandacode-host PATH: puts this run in its own cgroup task-<pid>-<rand>
# under the host's delegated cgroup, so oomd or the kernel can kill one task
# and not the host, then execs the real CLI found later on PATH.
# Outside the host (no $PANDA_TASK_CGROUP_ROOT) it just execs the CLI.
me=$(readlink -f "$0")
real=
IFS=: read -ra dirs <<<"${PATH-}"
for d in "${dirs[@]}"; do
  c=$d/$name
  if [ -x "$c" ] && [ ! -d "$c" ] && [ "$(readlink -f "$c")" != "$me" ]; then
    real=$c
    break
  fi
done
if [ -z "$real" ]; then
  echo "$name: not installed (no $name on PATH after the PandaOS shim)" >&2
  exit 127
fi

root=${PANDA_TASK_CGROUP_ROOT-}
if [ -n "$root" ] && [ -w "$root/cgroup.procs" ]; then
  task=$root/task-$$-$RANDOM$RANDOM
  if mkdir "$task" 2>/dev/null; then
    # OOM kills the whole task (CLI plus its children) together.
    echo 1 >"$task/memory.oom.group" 2>/dev/null || true
    if ! echo "$$" >"$task/cgroup.procs"; then
      rmdir "$task" 2>/dev/null || true
      echo "$name: running without a task cgroup" >&2
    fi
  fi
fi
exec "$real" "$@"
