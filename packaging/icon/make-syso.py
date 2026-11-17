"""Writes a Windows resource object (.syso) holding an application icon, for Go to link into the .exe.

Usage: python3 make-syso.py <icon.ico> <amd64|arm64> <out.syso>

`go build` links any *_windows_<arch>.syso next to main.go into Windows builds. The object is a COFF
file with one .rsrc section: an icon group (RT_GROUP_ICON, id 1, the first icon so Explorer uses it)
and one RT_ICON per image of the .ico. Each resource data entry's address is relocated
(ADDR32NB, section-relative) so the linker turns it into the image RVA.
"""
import struct
import sys

RT_ICON, RT_GROUP_ICON = 3, 14
LANG = 0x0409  # en-US
MACHINE = {'amd64': 0x8664, 'arm64': 0xAA64}
ADDR32NB = {'amd64': 0x0003, 'arm64': 0x0002}  # IMAGE_REL_AMD64_ADDR32NB / IMAGE_REL_ARM64_ADDR32NB


def read_ico(path):
    data = open(path, 'rb').read()
    reserved, kind, count = struct.unpack_from('<HHH', data, 0)
    if reserved != 0 or kind != 1 or count == 0:
        raise SystemExit(f'{path}: not an .ico file')
    images = []
    for i in range(count):
        w, h, colors, _, planes, bits, size, offset = struct.unpack_from('<BBBBHHII', data, 6 + 16 * i)
        images.append({'w': w, 'h': h, 'colors': colors, 'planes': planes, 'bits': bits, 'data': data[offset:offset + size]})
    return images


def align(n, a=8):
    return (n + a - 1) // a * a


def build_rsrc(images):
    """Returns (section bytes, offsets of the relocated OffsetToData fields)."""
    group = struct.pack('<HHH', 0, 1, len(images)) + b''.join(
        struct.pack('<BBBBHHIH', im['w'], im['h'], im['colors'], 0, im['planes'] or 1, im['bits'] or 32, len(im['data']), i + 1)
        for i, im in enumerate(images))
    # Leaves, in directory order: RT_ICON ids 1..n, then RT_GROUP_ICON id 1.
    leaves = [(RT_ICON, i + 1, im['data']) for i, im in enumerate(images)] + [(RT_GROUP_ICON, 1, group)]
    types = [(RT_ICON, [l for l in leaves if l[0] == RT_ICON]), (RT_GROUP_ICON, [l for l in leaves if l[0] == RT_GROUP_ICON])]

    def dir_size(n):
        return 16 + 8 * n

    # Layout: root dir, one name dir per type, one language dir per leaf, data entries, then data.
    off = dir_size(len(types))
    name_dir_off = {}
    for t, ls in types:
        name_dir_off[t] = off
        off += dir_size(len(ls))
    lang_dir_off = {}
    for t, ls in types:
        for l in ls:
            lang_dir_off[(t, l[1])] = off
            off += dir_size(1)
    entry_off = {}
    for t, ls in types:
        for l in ls:
            entry_off[(t, l[1])] = off
            off += 16
    data_off = {}
    for t, ls in types:
        for l in ls:
            off = align(off)
            data_off[(t, l[1])] = off
            off += len(l[2])
    sec = bytearray(align(off))

    def write_dir(at, entries):  # entries: (id, offset, is_dir)
        struct.pack_into('<IIHHHH', sec, at, 0, 0, 0, 0, 0, len(entries))
        for i, (ident, target, is_dir) in enumerate(entries):
            struct.pack_into('<II', sec, at + 16 + 8 * i, ident, target | (0x80000000 if is_dir else 0))

    write_dir(0, [(t, name_dir_off[t], True) for t, _ in types])
    relocs = []
    for t, ls in types:
        write_dir(name_dir_off[t], [(l[1], lang_dir_off[(t, l[1])], True) for l in ls])
        for l in ls:
            k = (t, l[1])
            write_dir(lang_dir_off[k], [(LANG, entry_off[k], False)])
            # IMAGE_RESOURCE_DATA_ENTRY: OffsetToData holds the section offset; the linker adds the RVA.
            struct.pack_into('<IIII', sec, entry_off[k], data_off[k], len(l[2]), 0, 0)
            relocs.append(entry_off[k])
            sec[data_off[k]:data_off[k] + len(l[2])] = l[2]
    return bytes(sec), relocs


def build_coff(arch, sec, relocs):
    header_size = 20 + 40
    raw_ptr = header_size
    reloc_ptr = raw_ptr + len(sec)
    sym_ptr = reloc_ptr + 10 * len(relocs)
    out = bytearray()
    out += struct.pack('<HHIIIHH', MACHINE[arch], 1, 0, sym_ptr, 1, 0, 0)
    out += struct.pack('<8sIIIIIIHHI', b'.rsrc', 0, 0, len(sec), raw_ptr, reloc_ptr, 0, len(relocs), 0,
                       0x40000040)  # initialized data, readable
    out += sec
    for r in relocs:
        out += struct.pack('<IIH', r, 0, ADDR32NB[arch])  # against symbol 0, the section
    out += struct.pack('<8sIhHBB', b'.rsrc', 0, 1, 0, 3, 0)  # static section symbol
    out += struct.pack('<I', 4)  # empty string table
    return bytes(out)


def main():
    if len(sys.argv) != 4 or sys.argv[2] not in MACHINE:
        raise SystemExit(__doc__)
    ico, arch, out = sys.argv[1:]
    sec, relocs = build_rsrc(read_ico(ico))
    with open(out, 'wb') as f:
        f.write(build_coff(arch, sec, relocs))


if __name__ == '__main__':
    main()
