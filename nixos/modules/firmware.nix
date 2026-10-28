# Curated firmware for the ISO and the prebuilt installed systems: common
# wifi chips, Realtek NICs, AMD/Intel GPUs, the wireless regulatory database
# and CPU microcode. It is copied out of linux-firmware (about 1.9 GB) so that
# no wifi laptop is stranded offline, without shipping all of it.
#
# Size, measured for linux-firmware 20261019 from its cache.nixos.org file
# listing: about 380 MB uncompressed. NixOS zstd-compresses firmware, which
# should give about 130 MB (sampled ratio 0.35).
{ lib, pkgs, ... }:
let
  src = pkgs.linux-firmware;

  # Paths under lib/firmware, as shell globs. A glob that matches nothing
  # fails the build (failglob), so an upstream rename gets noticed.
  paths = [
    # Intel wifi (top-level names are links into intel/iwlwifi)
    "intel/iwlwifi"
    "iwlwifi-*"
    # Qualcomm Atheros (no router/ARM-SoC parts: IPQ*, QCA4019, WCN3990, WCN6750)
    "ath9k_htc"
    "htc_*"
    "ath10k/QCA6174"
    "ath10k/QCA9377"
    "ath10k/QCA9887"
    "ath10k/QCA988X"
    "ath10k/QCA9888"
    "ath10k/QCA9984"
    "ath10k/QCA99X0"
    "ath11k/WCN6855"
    "ath11k/QCA2066"
    "ath11k/QCA6390"
    "ath11k/QCA6698AQ"
    "ath11k/QCN9074"
    # Realtek wifi and ethernet
    "rtw88"
    "rtw89"
    "rtlwifi"
    "rtl_nic"
    # Broadcom/Cypress (brcm links into cypress)
    "brcm"
    "cypress"
    # MediaTek mt76 PCIe/USB (no mt798x router or mt8xxx SoC parts)
    "mediatek/WIFI_*"
    "mediatek/BT_RAM_CODE_*"
    "mediatek/mt7925"
    "mediatek/mt7927"
    "mediatek/mt7915*"
    "mediatek/mt7916*"
    "mediatek/mt7610*"
    "mediatek/mt7650*"
    "mediatek/mt7662*"
    "mediatek/mt7663*"
    "mediatek/mt7668*"
    "mediatek/mt7601u.bin"
    "mt7*"
    # GPUs
    "amdgpu"
    "radeon"
    "i915"
  ];

  panda-firmware = pkgs.runCommand "panda-firmware-${src.version}" { } ''
    src=${src}/lib/firmware
    dst=$out/lib/firmware
    mkdir -p "$dst"
    cd "$src"
    shopt -s failglob
    # Dereference everything: linux-firmware dedups with symlinks (some into
    # directories), and copied store paths stay read-only otherwise.
    cp -rL --no-preserve=mode --parents ${lib.concatStringsSep " " paths} "$dst"/
    cd "$dst"

    # iwlwifi keeps every firmware API version (about 300 MB). Keep the 3
    # newest per device: the kernel loads the newest one it supports.
    for dev in $(ls intel/iwlwifi | sed -nE 's/^(iwlwifi-.+)-c?[0-9]+\.ucode$/\1/p' | sort -u); do
      ls intel/iwlwifi | grep -E "^$dev-c?[0-9]+\.ucode$" | sort -V | head -n -3 \
        | sed 's|^|intel/iwlwifi/|' | xargs -r rm -f
    done

  '';
in
{
  hardware.firmware = [ panda-firmware ];
  hardware.wirelessRegulatoryDatabase = true;
  # Above mkDefault: hardware-configuration.nix sets these with mkDefault to
  # enableRedistributableFirmware, which is false in VMs.
  hardware.cpu.intel.updateMicrocode = lib.mkOverride 900 true;
  hardware.cpu.amd.updateMicrocode = lib.mkOverride 900 true;
}
