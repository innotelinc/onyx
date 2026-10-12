#!/usr/bin/env python3
"""Verify staged NAS/platform packages without root or host service changes."""
import pathlib
import subprocess
import tempfile

root = pathlib.Path(__file__).resolve().parent.parent
for profile in ("nas", "platform"):
    with tempfile.TemporaryDirectory(prefix="onyx-nas-stage-") as stage:
        subprocess.run(
            ["bash", "scripts/onyx-install", "--profile", profile, "--destdir", stage,
             "--enable-pool", "--enable-firstboot"],
            cwd=root, check=True, stdout=subprocess.DEVNULL,
        )
        package = pathlib.Path(stage) / "usr/local"
        for service in ("core", "api", "shared", "storaged", "privd", "snapd", "backupd", "davd"):
            assert (package / f"lib/onyx/onyx-{service}").is_file(), service
        for service in ("vmm", "appd", "ai", "objectstore"):
            assert (package / f"lib/systemd/system/onyx-{service}.service").exists() == (profile == "platform"), service
        for service in ("pool", "firstboot"):
            assert (package / f"lib/systemd/system/onyx-{service}.service").exists(), service
        assert profile in (package / "share/onyx/web/config.js").read_text()
        assert "ONYX NAS" in (package / "share/onyx/web/index.html").read_text()
        print(f"{profile}: package staging passed")
