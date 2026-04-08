# SpankMac

<img src="assets/screenshot.png" width="300" alt="SpankMac Screenshot">

For Apple Silicon MacBooks. Detect physical hits using the internal accelerometer and play audio. [Based on taigrr/spank](https://github.com/taigrr/spank), added custom features, including a custom-made GUI.

## Prerequisites

- macOS on Apple Silicon (M-series)
- `sudo` access

## Quick Start

```bash
Download & unzip latest release.  
Drag into Applications folder.  
Run the Application and HF.  
```

## What to do if the app is not opening

If you experience "app is broken" or similar errors, you can use these workarounds:

- **Right-click -> Open**: Instead of double-clicking, right-click the app and select "Open." This adds an "Open anyway" button.
- **Terminal**: Run `xattr -d com.apple.quarantine /path/to/spankmac` to manually remove the security block.

## Modes

- **Default**: Random pain sounds.
- **Sexy**: Escalating intensity based on slap frequency.
- **Halo**: Halo sound effects.
- **Lizard**: Lizard.
- **Half-Life 2**: Sounds from Half-Life 2.

## Credits
Sensor reading and vibration detection ported from olvvier/apple-silicon-accelerometer.
Spank by [taigrr/spank](https://github.com/taigrr/spank)

## License

MIT
