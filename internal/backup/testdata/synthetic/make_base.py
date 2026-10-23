import struct, sys
buf = bytearray(4096)
struct.pack_into("<H", buf, 0, 0x1234)       # cfg_holder (any)
struct.pack_into("<H", buf, 2, 4096)         # cfg_size
struct.pack_into("<L", buf, 8, 0x0F060003)   # version 15.6.0.3
c=0
for b in buf[:4092]:
    c^=b
    for _ in range(8):
        c=(c>>1)^(-(c&1)&0xEDB88320)
struct.pack_into("<L", buf, 0xFFC, (~c)&0xffffffff)
out = bytearray(buf[0:2])
for i in range(2, len(buf)):
    out.append((buf[i] ^ (0x5A + i)) & 0xff)
open(sys.argv[1], "wb").write(out)
