"""Build a fake release tarball: mktar.py OUT MODE VERSION.

MODE: ok | dotdot | dotdot_cflk | notcflk | symlink. The fake cflk prints the bare version for any
arguments.
"""
import io
import sys
import tarfile

out, mode, version = sys.argv[1:4]
script = ("#!/bin/sh\necho %s\n" % version).encode()


def add(archive, name, body=script):
    info = tarfile.TarInfo(name)
    info.size = len(body)
    info.mode = 0o755
    archive.addfile(info, io.BytesIO(body))


def link(archive, name):
    info = tarfile.TarInfo(name)
    info.type = tarfile.SYMTYPE
    info.linkname = "/bin/sh"
    archive.addfile(info)


with tarfile.open(out, "w:gz") as archive:
    if mode == "ok":
        add(archive, "cflk")
        add(archive, "README.md", b"readme\n")
    elif mode == "dotdot":
        add(archive, "cflk")
        add(archive, "../evil")
    elif mode == "dotdot_cflk":
        add(archive, "../cflk")
    elif mode == "notcflk":
        add(archive, "bin/cflk")
    elif mode == "symlink":
        link(archive, "cflk")
    else:
        sys.exit("unknown mode " + mode)
