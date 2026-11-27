"""Extend a Docker Engine default seccomp profile for nested Bubblewrap."""

import json
import sys
from pathlib import Path


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: runner-seccomp.py /path/to/docker-default.json")

    profile = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
    if profile.get("defaultAction") != "SCMP_ACT_ERRNO" or not profile.get("syscalls"):
        raise SystemExit("expected a deny-by-default Docker Engine seccomp profile")

    # Bubblewrap creates unprivileged namespaces and mounts from inside them.
    # The kernel still checks namespace ownership and capabilities on each call.
    profile["syscalls"].append({
        "names": ["clone", "mount", "pivot_root", "sethostname", "setns", "umount2", "unshare"],
        "action": "SCMP_ACT_ALLOW",
        "comment": "Allow Bubblewrap to set up nested unprivileged namespaces",
    })
    json.dump(profile, sys.stdout, indent=2)
    print()


if __name__ == "__main__":
    main()
