"""Shared process isolation for local verification, never trading execution."""
from contextlib import contextmanager
import fcntl
import hashlib
import os
from pathlib import Path


@contextmanager
def verification_lock(root):
    """Serialize gate entry points; an inherited, validated FD allows nesting.

    The file is never unlinked: unlinking a held lock permits a second inode
    and two simultaneous owners. Process exit releases flock automatically.
    """
    key = hashlib.sha256(str(Path(root).resolve()).encode()).hexdigest()[:20]
    # Stable across cleared environments and different TMPDIR settings.
    path = Path("/tmp").resolve() / ("lip-verification-" + key + ".lock")
    with path.open("a+") as lock:
        inherited = os.environ.get("LIP_VERIFY_LOCK_FD", "")
        if inherited.isdigit():
            fd = int(inherited)
            valid = False
            try:
                stat = os.fstat(fd)
                if (stat.st_dev, stat.st_ino) == (path.stat().st_dev, path.stat().st_ino):
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    valid = True
            except (OSError, ValueError):
                pass
            if valid:
                yield fd
                return
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise RuntimeError("another verification owns " + str(path)) from exc
        lock.seek(0)
        lock.truncate()
        lock.write(str(os.getpid()) + "\n")
        lock.flush()
        yield lock.fileno()


def local_environment():
    """Explicit offline environment. No exchange credentials are inherited."""
    home = str(Path.home())
    env = {"PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
           "HOME": home, "GOMODCACHE": home + "/go/pkg/mod",
           "GOCACHE": home + "/Library/Caches/go-build",
           "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local",
           "GOENV": "off", "GOWORK": "off",
           "GOMAXPROCS": "2", "GOFLAGS": "-p=2", "CGO_ENABLED": "0",
           "PYTHONUNBUFFERED": "1", "PYTHONDONTWRITEBYTECODE": "1"}
    for name in ("GOCACHE", "GOMODCACHE", "GOMAXPROCS", "TMPDIR"):
        if name in os.environ:
            env[name] = os.environ[name]
    return env
