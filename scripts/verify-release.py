"""Validate the two release assets and their exact checksums without extraction."""
import hashlib
import pathlib
import struct
import sys
import tarfile

version, folder = sys.argv[1:]
root = pathlib.Path(folder)
sums = {}
for line in (root / 'SHA256SUMS').read_text(encoding='utf-8').splitlines():
    digest, name = line.split()
    assert name not in sums, 'Duplicate checksum entry'
    sums[name] = digest
assert len(sums) == 2, 'Expected exactly two architecture archives'
for arch, machine in [('amd64', 62), ('arm64', 183)]:
    name = f'pikpak-vault-{version}-linux-{arch}.tar.gz'
    data = (root / name).read_bytes()
    assert hashlib.sha256(data).hexdigest() == sums[name], 'Checksum mismatch'
    with tarfile.open(root / name) as archive:
        names = set()
        for item in archive:
            path = pathlib.PurePosixPath(item.name)
            assert not path.is_absolute() and '..' not in path.parts
            assert item.isdir() or item.isfile(), 'Special archive member'
            assert item.name not in names, 'Duplicate archive member'
            names.add(item.name)
        assert {'vault', 'README.md', 'deploy/install.sh', 'deploy/pikpak-vault-update.service'} <= names
        assert not any(n.startswith(('data/', 'artifacts/')) or n.endswith(('.key', '.db', '.zip')) for n in names)
        binary = archive.extractfile('vault').read(64)
        assert binary[:6] == b'\x7fELF\x02\x01' and struct.unpack_from('<H', binary, 18)[0] == machine
    print(f'Validated {name}: SHA-256, paths, permissions, Linux {arch}')
