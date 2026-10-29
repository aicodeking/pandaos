# bench/compare

Boots PandaOS headless and stock cloud images of other distros under QEMU+KVM with identical
resources (2 vCPU, 2048 MB, 20 GB virtio disk, user-mode net, UEFI) and measures idle RAM,
processes, running services, disk used, image size and boot-to-ssh time. Run by
`.github/workflows/compare.yml` (manual and weekly); results and methodology: `docs/comparison.md`.

- `images.json`: pinned image URLs and checksums, from each project's official sums file.
- `pandaos-image.nix`: the PandaOS headless qcow2 (how it differs from an installed box is at the top).
- `run.py`: the driver; `test_run.py` self-tests its parsing (`python3 bench/compare/test_run.py`).
