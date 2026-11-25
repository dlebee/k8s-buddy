# /// script
# requires-python = ">=3.9"
# dependencies = ["ds-store==1.3.4", "mac-alias==2.2.4"]
# ///
"""Writes the Finder layout of the DMG window into <volume>/.DS_Store.

Usage: uv run make-dsstore.py /Volumes/k8s-buddy   (make-dmg-assets.sh does this)

The volume must already hold .background/background.tiff: Finder stores the picture as an alias to
that file, so it has to exist while this runs. The numbers match background.svg.
"""
import sys

from ds_store import DSStore
from mac_alias import Alias

WIDTH, HEIGHT = 660, 400  # the picture, in points
TITLE_BAR = 28
ICON_SIZE = 128
ICONS = {"k8s-buddy.app": (180, 190), "Applications": (480, 190)}

volume = sys.argv[1].rstrip("/")
alias = Alias.for_file(volume + "/.background/background.tiff")

with DSStore.open(volume + "/.DS_Store", "w+") as d:
    d["."]["vSrn"] = ("long", 1)
    d["."]["bwsp"] = {
        "WindowBounds": "{{200, 120}, {%d, %d}}" % (WIDTH, HEIGHT + TITLE_BAR),
        "ShowToolbar": False,
        "ShowStatusBar": False,
        "ShowPathbar": False,
        "ShowTabView": False,
        "ShowSidebar": False,
        "ContainerShowSidebar": False,
        "PreviewPaneVisibility": False,
        "SidebarWidth": 180,
    }
    d["."]["icvp"] = {
        "viewOptionsVersion": 1,
        "backgroundType": 2,  # a picture
        "backgroundImageAlias": alias.to_bytes(),
        "backgroundColorRed": 1.0,
        "backgroundColorGreen": 1.0,
        "backgroundColorBlue": 1.0,
        "arrangeBy": "none",
        "gridOffsetX": 0.0,
        "gridOffsetY": 0.0,
        "gridSpacing": 100.0,
        "iconSize": float(ICON_SIZE),
        "textSize": 13.0,
        "labelOnBottom": True,
        "showIconPreview": False,
        "showItemInfo": False,
        "scrollPositionX": 0.0,
        "scrollPositionY": 0.0,
    }
    for name, position in ICONS.items():
        d[name]["Iloc"] = position
