#!/usr/bin/env python3
"""Extract exact executable metadata on a compatible machine, without approving it."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys


def sha256_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("os", choices=("linux", "darwin"))
    parser.add_argument("arch", choices=("amd64", "arm64"))
    args = parser.parse_args()
    machine = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    if (platform.system().lower(), machine) != (args.os, args.arch):
        parser.error("run extraction on the executable's native OS/architecture")
    binary = args.binary.absolute()
    if not binary.is_file() or binary.is_symlink() or not os.access(binary, os.X_OK):
        parser.error("binary must be a regular executable, not a symlink")
    checksum = sha256_file(binary)
    # Keep these exact bytes: the SDK emits protobuf JSON with camelCase keys.
    manifest_bytes = subprocess.check_output([str(binary), "manifest"], timeout=30)
    manifest = json.loads(manifest_bytes)
    if manifest.get("checksum") != checksum:
        parser.error("extracted manifest checksum does not match the executable")
    if manifest.get("pluginId") != "bloem.storage.bookwarehouse" or manifest.get("version") != "0.4.0":
        parser.error("unexpected plugin identity or version")
    if manifest.get("siloApiVersion") != "v1":
        parser.error("unsupported SDK API version")
    if {"os": args.os, "arch": args.arch} not in manifest.get("supportedPlatforms", []):
        parser.error("extracted manifest does not support this platform")
    if manifest.get("httpRoutes") or manifest.get("assets"):
        parser.error("native storage artifact must not expose routes or assets")
    # The only public surface is request_router.v1, one capability per content kind.
    capabilities = sorted(
        (c.get("type"), c.get("id")) for c in manifest.get("capabilities", [])
    )
    if capabilities != [("request_router.v1", "audiobooks"), ("request_router.v1", "ebooks")]:
        parser.error("unexpected public capabilities: want request_router.v1 ebooks and audiobooks")
    # Connections carry no address or key; the host skips keyless ones without these flags.
    for c in manifest.get("capabilities", []):
        router = c.get("requestRouter") or {}
        if router.get("apiKeyOptional") is not True or router.get("baseUrlOptional") is not True:
            parser.error(f"capability {c.get('id')} must declare apiKeyOptional and baseUrlOptional")
    # Reject a concurrent replacement rather than emit mismatched release files.
    if sha256_file(binary) != checksum:
        parser.error("executable changed during extraction")
    key = f"bookwarehouse-0.4.0-{args.os}-{args.arch}-{checksum[:12]}"
    approval = {key: {"manifest": manifest, "checksum": checksum, "os": args.os, "arch": args.arch}}
    output = binary.parent
    (output / "manifest.json").write_bytes(manifest_bytes)
    (output / "SHA256SUMS").write_text(f"{checksum}  {binary.name}\n", encoding="utf-8")
    (output / "native-storage-approvals.example.json").write_text(
        json.dumps(approval, indent=2) + "\n", encoding="utf-8"
    )
    print(f"Packaged {binary}: SHA-256 {checksum}")
    print(f"Approval example only (operator review required): {key}")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"packaging failed: {error}", file=sys.stderr)
        sys.exit(1)
