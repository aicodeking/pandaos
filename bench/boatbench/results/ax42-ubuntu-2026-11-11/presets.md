# boatbench: PandaOS by boatd preset (MEASURED)

Each column is a separate run on the harness-declared host(s) below, so run-to-run variance is in the differences too. median [IQR] n=samples.

| Metric | Unit | Better | trusted | trusted-warm | trusted-writeback | untrusted |
|---|---|---|---|---|---|---|
| (a) Node.js build/test loop: PTS node-web-tooling | runs/s | higher | 21.13 [21.09-21.19] n=9 | - | - | 21.21 [21.18-21.30] n=9 |
| (b) API round-trip baseline: authenticated GET of the VM, p50 of 25 | ms | lower | 0.283 [0.276-0.304] n=3 | 0.240 [0.240-0.277] n=3 | - | 0.286 [0.280-0.294] n=3 |
| (b) create -> exec-ready | s | lower | 1.40 [1.38-1.41] n=8 | 0.318 [0.310-0.327] n=3 | 1.73 [1.72-5.43] n=3 | 1.39 [1.39-1.40] n=8 |
| (b) exec round trip `true`, p50 of 50 | ms | lower | 2.29 [2.23-2.30] n=3 | 2.37 [2.36-2.40] n=3 | - | 2.21 [2.19-2.24] n=3 |
| (b) fork -> fork exec-ready | s | lower | 1.24 [1.21-1.36] n=3 | 0.626 [0.571-0.663] n=3 | - | 0.556 [0.552-0.602] n=3 |
| (b) stop (until stopped) | s | lower | 0.617 [0.521-0.628] n=3 | 0.604 [0.580-0.634] n=3 | - | 0.627 [0.615-0.642] n=3 |
| (b) resume -> exec-ready | s | lower | 0.256 [0.238-0.265] n=3 | 0.238 [0.216-0.239] n=3 | - | 0.211 [0.196-0.222] n=3 |
| (b) snapshot (until usable) | s | lower | 1.79 [1.51-2.17] n=3 | 0.662 [0.662-0.671] n=3 | - | 1.67 [1.47-1.73] n=3 |
| (b) new-from-snapshot -> exec-ready | s | lower | 0.220 [0.220-0.225] n=3 | 0.246 [0.241-0.254] n=3 | - | 0.227 [0.201-0.232] n=3 |
| (c) 10 concurrent creates, wall to all exec-ready | s | lower | 2.99 [2.94-3.03] n=3 | 1.70 [1.59-1.72] n=3 | - | 4.25 [4.18-4.30] n=3 |
| (d) disk seq write 1M | MB/s | higher | 1,863 [1,811-2,053] n=3 | - | 2,819 [2,728-3,330] n=3 | 351.1 [334.1-359.9] n=3 |
| (d) disk seq read 1M | MB/s | higher | 5,903 [5,175-6,859] n=3 | - | 9,959 [8,706-10,415] n=3 | 5,265 [4,366-5,830] n=3 |
| (d) disk rand write 4k | IOPS | higher | 147,933 [147,926-148,402] n=3 | - | 143,135 [141,590-144,300] n=3 | 84,281 [82,666-85,408] n=3 |
| (d) disk rand read 4k | IOPS | higher | 82,523 [81,494-129,108] n=3 | - | 174,697 [174,570-175,520] n=3 | 200,768 [199,228-202,214] n=3 |
| (d) PTS fio job: rand write 4k | IOPS | higher | 171,442 [171,064-171,514] n=3 | - | 168,718 [168,028-169,090] n=3 | 159,704 [158,989-160,201] n=3 |
| (d) PTS fio job: rand read 4k | IOPS | higher | 155,795 [119,692-166,690] n=3 | - | 179,819 [178,664-179,845] n=3 | 202,589 [129,914-206,160] n=3 |
| (d) rand write 4k, fsync after every write, iodepth 1 | IOPS | higher | 23,942 [23,907-24,044] n=3 | - | 38.00 [38.00-38.50] n=3 | 25,190 [25,098-25,370] n=3 |
| (d) seq write 1M, fsync at the end | MB/s | higher | 2,640 [2,494-2,683] n=3 | - | 3,517 [2,854-4,036] n=3 | 686.9 [624.8-1,014] n=3 |
| (e) download Go 1.25.0 tarball, 59.7 MB (dl.google.com) | Mbit/s | higher | 812.4 [698.3-837.6] n=3 | - | - | 519.0 [489.3-651.0] n=3 |
| (f) cold `npm ci` of pinned project | s | lower | 2.56 [2.52-2.62] n=3 | - | - | 2.51 [2.49-2.52] n=3 |

| | trusted | trusted-warm | trusted-writeback | untrusted |
|---|---|---|---|---|
| boatd options | {"cgroupParent":"boat-bench","reflink":"always","bootTimeoutS":120,"defaultImage":"slim","memOvercommit":8,"cpuOvercommit":12,"defaultTeam":{"maxVMs":16}} | {"cgroupParent":"boat-bench","reflink":"always","bootTimeoutS":120,"defaultImage":"slim","memOvercommit":8,"cpuOvercommit":12,"defaultTeam":{"maxVMs":16},"warmPool":true} | {"cgroupParent":"boat-bench","reflink":"always","bootTimeoutS":120,"defaultImage":"slim","memOvercommit":8,"cpuOvercommit":12,"defaultTeam":{"maxVMs":16},"diskCache":"Writeback"} | {"cgroupParent":"boat-bench","reflink":"always","bootTimeoutS":120,"defaultImage":"slim","memOvercommit":8,"cpuOvercommit":12,"defaultTeam":{"maxVMs":16},"untrustedTenants":true,"btrfsQgroups":true,"storageOvercommit":1,"egressMbit":1000,"ingressMbit":1000,"ioCost":true,"ioCostQoS":"rpct=95 rlat=5000 wpct=95 wlat=10000 min=25 max=400","ioMaxMBps":4000,"ioMaxIOPS":200000,"ctPerVM":4096,"diskEngine":"Sync","hostDirtyRatio":20,"warmPool":false,"ksmTeam":""} |
| Firecracker cache_type | Unsafe (boatd default diskCache) | Unsafe (boatd default diskCache) | Writeback (boatd.json diskCache) | Unsafe (boatd default diskCache) |
| VM vCPU seen | 4 | 4 | 4 | 4 |
| Failed steps | none | none | none | none |
| Host (harness-declared) | Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB, 2x512 GB NVMe mdadm RAID1; boat state = btrfs loop file on ext4 on RAID1; preset trusted; 16 CPUs online | Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB, 2x512 GB NVMe mdadm RAID1; boat state = btrfs loop file on ext4 on RAID1; preset trusted-warm; 16 CPUs online | Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB, 2x512 GB NVMe mdadm RAID1; boat state = btrfs loop file on ext4 on RAID1; preset trusted-writeback; 16 CPUs online | Hetzner AX42, Ryzen 7 PRO 8700GE (Zen 4, 8c/16t), 64 GB, 2x512 GB NVMe mdadm RAID1; boat state = btrfs loop file on ext4 on RAID1; preset untrusted; 8 CPUs online |
| Run URL | - | - | - | - |
