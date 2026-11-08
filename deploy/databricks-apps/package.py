"""Package a static Linux Go binary into Apps files smaller than 10 MB."""
import argparse
import gzip
import hashlib
import json
from pathlib import Path
import shutil
import struct

CHUNK_SIZE = 8_000_000


def binary_architecture(binary: bytes) -> str:
    if len(binary) < 64 or binary[:6] != b"\x7fELF\x02\x01":
        raise ValueError("expected a 64-bit little-endian Linux ELF binary")
    architectures = {62: "amd64", 183: "arm64"}
    machine = struct.unpack_from("<H", binary, 18)[0]
    if machine not in architectures:
        raise ValueError("only Linux amd64 and arm64 are supported")
    offset = struct.unpack_from("<Q", binary, 32)[0]
    entry_size, count = struct.unpack_from("<HH", binary, 54)
    if entry_size < 4 or offset + entry_size * count > len(binary):
        raise ValueError("invalid ELF program headers")
    for index in range(count):
        if struct.unpack_from("<I", binary, offset + index * entry_size)[0] == 3:
            raise ValueError("binary uses a dynamic interpreter; build a statically linked CGO binary")
    return architectures[machine]


def package(binary_path: Path, output: Path) -> dict:
    binary = binary_path.read_bytes()
    architecture = binary_architecture(binary)
    if output.exists():
        raise ValueError("output directory already exists; choose a fresh package directory")
    output.mkdir(parents=True)
    compressed = gzip.compress(binary, mtime=0)
    chunks = []
    for index, start in enumerate(range(0, len(compressed), CHUNK_SIZE)):
        name = f"binary.part-{index:04d}"
        (output / name).write_bytes(compressed[start:start + CHUNK_SIZE])
        chunks.append(name)
    manifest = {"format_version": 1, "os": "linux", "architecture": architecture,
                "sha256": hashlib.sha256(binary).hexdigest(), "size": len(binary), "chunks": chunks}
    (output / "binary.json").write_text(json.dumps(manifest, indent=2) + "\n")
    source = Path(__file__).resolve().parent
    for name in ("launch.py", "app.yaml", "databricks.yml", "config.json"):
        shutil.copyfile(source / name, output / name)
    return manifest


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    print(json.dumps(package(args.binary, args.output), indent=2))
