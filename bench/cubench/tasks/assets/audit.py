#!/usr/bin/env python3
"""Who wrote what: a fanotify watcher (run as root) and the checker for its log.

  audit.py watch ROOT LOG
      Log every create / rename / close-after-write under ROOT (on ROOT's
      filesystem; it must not be a btrfs subvolume), one line each:
      "<pid>\\t<comm>\\t<event>\\t<path>". Writes "ready" once the mark is set.
      The log lives outside the agent's reach (root-owned, /var/tmp).
  audit.py check LOG REGEX PATH...
      Passes when, for every PATH, the last create/rename-to/write event on it
      came from a process whose comm matches REGEX (e.g. the editor), so a
      shell (bash, mv, cp, python3, ...) cannot have produced the result.

fanotify reports the writing process (tgid) with every event, which inotify
does not. Needs Linux >= 5.9 (FAN_REPORT_DFID_NAME) and CAP_SYS_ADMIN.
"""
import ctypes
import os
import re
import struct
import sys

FAN_CLOEXEC, FAN_REPORT_DFID_NAME = 0x1, 0xC00
FAN_MARK_ADD, FAN_MARK_FILESYSTEM = 0x1, 0x100
EVENTS = {0x8: "close_write", 0x40: "moved_from", 0x80: "moved_to", 0x100: "create", 0x200: "delete"}
FAN_ONDIR = 0x40000000
INFO_DFID_NAME = 2
WRITES = {"close_write", "moved_to", "create"}


def watch(root, log_path):
    libc = ctypes.CDLL(None, use_errno=True)
    libc.fanotify_mark.argtypes = [ctypes.c_int, ctypes.c_uint, ctypes.c_uint64, ctypes.c_int, ctypes.c_char_p]
    fd = libc.fanotify_init(FAN_CLOEXEC | FAN_REPORT_DFID_NAME, os.O_RDONLY)
    if fd < 0:
        raise OSError(ctypes.get_errno(), "fanotify_init")
    mask = sum(EVENTS) | FAN_ONDIR
    if libc.fanotify_mark(fd, FAN_MARK_ADD | FAN_MARK_FILESYSTEM, mask, -100, root.encode()) < 0:
        raise OSError(ctypes.get_errno(), "fanotify_mark")
    mount_fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
    root = os.path.realpath(root).rstrip("/") + "/"
    paths, me = {}, os.getpid()
    log = open(log_path, "a", buffering=1)
    log.write("ready\n")

    def dir_path(handle):
        if handle not in paths:
            try:
                hfd = libc.open_by_handle_at(mount_fd, ctypes.c_char_p(handle), os.O_RDONLY)
                paths[handle] = os.readlink(f"/proc/self/fd/{hfd}") if hfd >= 0 else None
                if hfd >= 0:
                    os.close(hfd)
            except OSError:
                paths[handle] = None
        return paths[handle]

    while True:
        buf = os.read(fd, 65536)
        off = 0
        while off + 24 <= len(buf):
            ev_len, _, _, meta_len, mask, _, pid = struct.unpack_from("=IBBHQii", buf, off)
            info, end = off + meta_len, off + ev_len
            off = end
            if pid == me:
                continue
            while info + 4 <= end:
                itype, _, ilen = struct.unpack_from("=BBH", buf, info)
                if itype == INFO_DFID_NAME:
                    hbytes = struct.unpack_from("=I", buf, info + 12)[0]
                    handle = buf[info + 12:info + 20 + hbytes]
                    name = buf[info + 20 + hbytes:info + ilen].split(b"\0", 1)[0].decode(errors="replace")
                    d = dir_path(bytes(handle))
                    path = os.path.join(d, name) if d else None
                    if path and (path + "/").startswith(root):
                        try:
                            with open(f"/proc/{pid}/comm") as f:
                                comm = f.read().strip()
                        except OSError:
                            comm = "?"
                        for bit, ev in EVENTS.items():
                            if mask & bit:
                                log.write(f"{pid}\t{comm}\t{ev}\t{path}\n")
                info += max(ilen, 4)


def check(log_path, regex, paths):
    try:
        with open(log_path) as f:
            rows = [line.rstrip("\n").split("\t") for line in f if line.count("\t") == 3]
    except FileNotFoundError:
        print(f"no audit log {log_path}")
        return False
    ok = True
    for p in paths:
        p = os.path.realpath(os.path.expanduser(p))
        last = [r for r in rows if r[3] == p and r[2] in WRITES]
        if not last:
            print(f"{p}: no write event recorded")
            ok = False
        elif not re.search(regex, last[-1][1]):
            print(f"{p}: last written by {last[-1][1]} (pid {last[-1][0]}, {last[-1][2]}), want /{regex}/")
            ok = False
        else:
            print(f"{p}: written by {last[-1][1]} ({last[-1][2]})")
    return ok


if __name__ == "__main__":
    if len(sys.argv) >= 4 and sys.argv[1] == "watch":
        watch(sys.argv[2], sys.argv[3])
    elif len(sys.argv) >= 5 and sys.argv[1] == "check":
        sys.exit(0 if check(sys.argv[2], sys.argv[3], sys.argv[4:]) else 1)
    else:
        sys.exit(__doc__)
