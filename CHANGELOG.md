# Changelog

What changed in each release of Shark BiteTorrent. The release notes on GitHub are taken from this file, so every release's section should say what users will notice.

## [Unreleased]

### Added
- **Auto verify** (Settings → Desktop): before the malware scan, a finished download's files are re-checked against the torrent if something could have damaged them: its drive disconnected, a read or write failed, or the app didn't shut down normally while it was downloading. Damaged pieces are downloaded again, and the complete, verified files are scanned. The files panel says why it was verified. It can also verify every download, or be turned off; verifying everything reads each download back from the drive once more, which is slow for large files on a network drive, so it's not the default.
- After **Verify files** finds damaged pieces, the files are scanned for malware again once they've been downloaded again (the scan may have seen the damaged data).

### Fixed
- When a network drive (a mapped drive on Windows, say) disconnected and came back, the torrent resumed but nothing it downloaded was saved: it kept using the connection that had dropped, so every write failed and it downloaded the same pieces over and over. It now reconnects to its files once the drive is back, and a read or write that fails is retried on a fresh connection.
- **Remove with files** while a torrent's drive was disconnected said it wasn't removed, but the torrent still disappeared from the list the next time the app started. It now stays, untouched, and the message says plainly that nothing was removed.
- Pieces already downloaded are no longer forgotten when the drive goes away while they're being checked.
- A torrent that needed pieces again (after **Verify files** found damaged ones, or its drive came back) could sit at 0 peers indefinitely, and a magnet link without trackers restored when the app started could stay at "Fetching metadata"; only restarting the app helped. The app now looks for peers itself when a torrent needs data and has none connected.
- A torrent showed as finished (and its malware scan started) as soon as every byte had arrived, even while some pieces were still waiting for their check, which after a restart can be hundreds. It now shows **Checking** until they're all confirmed, and is scanned only then.
- **Verify files** sometimes reported "all pieces match" when it had found damaged ones: they were already being downloaded again by the time it counted. It now reports every damaged piece it found, and once they're downloaded again says so with a check mark instead of a red warning.
- A download to a network drive could finish with damaged pieces if the drive disconnected partway: the computer reported pieces as written while still holding them in its cache, the app recorded them as finished, and the cached data was lost with the connection (a Linux ISO failed its checksum until **Verify files** re-downloaded 11 pieces). Each piece is now confirmed to be on the drive before it counts as finished. This costs about 10% of write speed to a NAS.

## [0.2.39] - 2026-10-21

### Added
- macOS and the Linux tarball: **Update now** installs updates in place and restarts the app, as on Windows, instead of saving the file for you to install. The download is checked against the release's signed checksums first, and on macOS the new app must be Shark BiteTorrent, at that version, and launchable (its built-in signature checks out; that signature isn't a publisher's, so the signed checksums are what prove where it came from). (The pacman package still gets **Download (verified)**: pacman installs it.)
- Updates are now checked for when the app starts (if it hasn't checked in the last hour) and about once a day while it runs, instead of only at start once a day: an app left running for weeks didn't hear about new versions.

### Fixed
- **Malware check:** a program named like a film ("Movie 2026 1080p h264.exe", or a program in a torrent named like one) is now flagged as a disguised program. One strong sign is enough (a resolution like 1080p, a rip source like WEB-DL, an episode number), or two weaker ones (a codec, BluRay, HDR, 4K), so video tools named after a codec aren't flagged; full-disc rips (.iso) aren't either. Such a fake got no warning: it had no double extension and no video beside it, and ClamAV, scanning all 1.2 GB of it with up-to-date definitions, found nothing. A clean scan now says "found no known threats", and doesn't show a green tick next to a warning. A torrent that gets such a warning is now paused as soon as its file list arrives, before it really downloads; press **Resume** in the app's window if you trust it, and it isn't paused for it again. The phone's remote control and **Resume all** don't resume such a torrent, since they don't show the warning. Every torrent's download now waits until its file list has been checked.
- Windows: the window's web view was told to rescale itself when it moved to a monitor with different display scaling, on top of the app's own handling (a bug in the WebView2 library, go-webview2 1.0.23). That could size or scale the window's contents wrongly on multi-monitor setups.
- Security: built with Go 1.27.3, which distrusts compromised intermediate certificates when checking HTTPS connections (GO-2026-6809). Updates are also checked against the release signature, so this didn't affect them.
- Linux: after running the downloaded tarball version and making it the default for magnet links, installing the pacman package didn't take over: the app menu and magnet links kept opening the old copy, because its menu entry hid the package's, and **Start when I sign in** kept starting the old copy too. The installed app now removes that leftover menu entry and takes over sign-in start when it starts. A tarball copy you move to another folder updates its entry too.
- macOS: the app couldn't be quit, and once its window was closed it couldn't be brought back. Closing the window and Cmd+Q / Dock → Quit only hid the window; the process kept running (the Dock icon stayed) until it was force-quit, and clicking the Dock icon did nothing. Closing the window now hides the app like other Mac apps that keep running, clicking the Dock icon brings it back, and Cmd+Q / Dock → Quit exit the process (the fix for the process staying behind contributed by [@a3app](https://github.com/a3app)). Quitting while downloads are being saved no longer shows the spinning cursor: the app saves them before it stops. (The "keep running in the tray" setting doesn't apply on macOS, which has no tray icon yet.)
- **Privacy:** HTTPS certificates are now checked inside the app, against Mozilla's trusted root certificates built into it, instead of by the operating system. On macOS and Windows the system's certificate checker can go online by itself, outside the VPN, to fetch revocation data or missing certificates, which could tell your network which sites the app talks to. (Certificate authorities you added to the system yourself, such as a company proxy's, aren't trusted by the app, and a server that doesn't send its full certificate chain now fails to connect.)
- A download folder on a network drive or disk that isn't connected now gives a clear error ("isn't available… connect it") instead of a confusing one, and nothing is downloaded elsewhere. (One case the app can't tell: a torrent added for the first time while a drive set up outside /etc/fstab is away, since its empty folder looks like any folder.) On Linux, where an unmounted drive leaves an empty folder behind, the app used to create the folders there and download onto the computer's own disk instead; a torrent resuming while its drive was gone did the same. A torrent whose drive goes away (or is away when the app starts) now pauses itself, including drives set up outside /etc/fstab (a systemd mount, autofs, mounted by hand), whose empty folder used to look like the drive (once the app has seen the drive connected: it notes where it was mounted), shown as blocked with the reason, and carries on by itself when the drive is back. Before, it kept downloading and threw the data away, and when the drive came back it downloaded everything again over the files already there. A network drive set to connect when used that can't (a NAS that's off) now gives the same clear error instead of "mkdir …: operation not permitted". Starting the app while the drive was away also made every torrent on it refuse its own files afterwards ("already exists"); that's fixed too. A torrent whose writing fails for another reason (a full disk, say) now pauses with the reason instead of sitting at 0 B/s. **Verify files** and **Remove with files** now say the drive isn't connected and change nothing (the torrent stays in the list with its progress), instead of marking everything as damaged or forgetting which files were the torrent's. On Linux, a download folder reached through a link (say ~/Media pointing at the drive's folder) is checked too. A download folder inside your home folder is still created along with any missing folders above it (the default Downloads/Torrents on a system without a Downloads folder).
- A magnet link or a relaunch that arrives while the app is quitting is ignored (it would have been lost with the quitting copy); open it again once the app has quit.
- Quitting while a download is saving to a slow network drive could leave the window frozen on screen, sometimes for a long time. The window now goes away at once, and the app finishes saving for up to 40 seconds before it exits; anything not saved by then is checked and downloaded again next time.
- macOS: the app needs macOS 13 (Ventura) or later, and now says so. It claimed to run on 10.13, but the Go version it's built with needs 13, and parts of it were built for the newest macOS, so on older versions it could fail to start.
- Linux: a copy of the app run from another folder (a second download, a test build) no longer takes over magnet links and .torrent files at startup while the copy they open is still there, and a copy in a folder any user can write to (such as /tmp) never does: another user could later put a program at that path.
- Linux: with **Closing the window keeps Shark BiteTorrent running in the tray** on (the default), being told to quit by the system (logging out, `kill`, Ctrl+C in a terminal) only hid the window, so the app was then killed without its normal shutdown, and pieces it hadn't saved yet were downloaded again. It now quits properly, like Quit in its tray menu.
- Search: **Check it's working** said Prowlarr or Jackett searched without the proxy when it had searched in the last minute or so, though it was using the proxy: it reused the connections it already had open through it, and the check only counted new ones. It now counts the traffic that goes through.
- macOS: the app asked for access to your Downloads folder every time it was first started, because it looked there for unfinished update downloads to clean up. It now only cleans up a download it knows it left unfinished, so it asks only if you download an update.
- Starting, adding or opening a torrent on a slow download folder (a busy NAS, say) could take a minute with no sign anything was happening. The buttons now say **Starting…** or **Adding…** until it's done.
- macOS: **Find Prowlarr or Jackett on this computer** didn't find the Prowlarr app, because it looked for Prowlarr's settings in the Linux location instead of ~/Library/Application Support/Prowlarr.
- **Remove with files** keeps a file that another torrent also owns even when that torrent's download folder was written differently (D:\dl and d:\dl, or a network drive's letter and its \\server path). Recovering from the app stopping in the middle of creating a file is stricter: a file of the right size that wasn't created by Shark BiteTorrent (a copy of the same download, say) is never taken over.

## [0.2.38] - 2026-10-20

### Fixed
- Quitting or restarting the app while downloading to a slow drive (a NAS, especially) lost part of the progress: pieces already downloaded and checked, but still being written, weren't recorded, so they downloaded again (in a test, 54% dropped to 37%). They're now written and recorded before the app closes, after checking each one, so a restart keeps what was downloaded.
- A torrent now only ever writes into, and deletes, files it created itself, tracked by the file's identity rather than its name. Before, a file it had made that you then deleted and replaced with your own (same name), or that another torrent of the same name shared, could still be written into or deleted with **Remove with files**. Removing with files also leaves alone folders that were already there (only the folders the torrent created are removed, if empty), and works for a magnet link whose file list hadn't arrived yet. Torrents you already have keep their files after the update.

## [0.2.37] - 2026-10-20

### Added
- Linux and macOS: **Download (verified)** in the update banner and Settings downloads the new version's file for your computer (the pacman package if you installed with pacman), checks it against the release signature like the Windows updater does, saves it in Downloads and tells you how to install it.
- **Start when I sign in** (Settings → Desktop): Shark BiteTorrent starts in the background at sign-in, in the tray, without opening its window. Works on Windows, Linux (KDE, GNOME and others) and macOS. In the Microsoft Store version, turn it on in Windows Settings → Apps → Startup (the option links there); that version opens its window at sign-in.

### Fixed
- **Remove with files** deleted the torrent's whole top-level folder, so it could take other things with it: another torrent with the same folder name, files you'd added, or (with the download folder set to a drive or your home folder) a library folder like Music. It now deletes only the torrent's own files, once nothing has them open (on network drives, deleting open files left stray ".__smbfile" files behind), then only the folders left empty.
- A torrent no longer writes into files that are already in the download folder when it starts (your own, or another torrent's with the same name): it stops and says why. Torrents you already have keep working after the update. A torrent you removed while keeping its files counts as new if you add it again, so it's refused over those files rather than overwriting them. (Unfinished downloads left as ".part" files by versions before 0.2.10 are no longer picked up.) Torrents with unsafe names (hidden names like ".ssh", Windows device names like CON, names ending in a dot or space) are refused.
- Magnet links and .torrent files opened from your browser or file manager now wait for you, with a banner to **Start** or **Remove** them: nothing is contacted (trackers, peers) until you press Start, so a web page can't start a download, or make the app announce itself to a tracker, on its own. Dropping a file on the window or adding from inside the app starts it right away, as before.
- **Check for updates** and **Update now** no longer use your normal connection by themselves when the VPN is down: the app asks first. It also refuses to "update" to an older version.
- Search: a Prowlarr or Jackett that isn't on this computer, your home network or Tailscale (a seedbox, say) was contacted over your normal connection. It now goes through the VPN. Downloading a result through a redirect no longer passes your Prowlarr/Jackett API key on to the site it redirects to.
- HTTPS trackers' certificates are now checked (the torrent library skipped the check), with or without the VPN, so nobody in between can read a private tracker's passkey. Tracker addresses in the app's logs no longer include passkeys.
- The app's window is now restricted to the app's own content (a Content-Security-Policy), so it can never load anything from the internet, on any platform.
- The check for disguised programs catches more tricks: names ending in a dot or space ("movie.mp4.exe."), invisible characters that reverse text so "gpj.exe" shows as "exe.jpg" (also removed from the names the app shows), and more risky file types (.url, .scf, .chm, disk images, macro documents, scripts).
- Windows: **Update now** installs into the folder the app is in. If you'd installed it somewhere other than the default, updates used to go to the default folder, leaving the old copy behind. The installer also waits for the app to close before forcing it, so it doesn't interrupt saving your torrent list.
- Files you tick after a torrent has finished are now scanned for malware (and marked as downloaded from the internet) too; before, only what was there the first time was. Files are marked as soon as they're created. After updating, finished torrents are checked once more.
- Settings → VPN: **WireGuard config file** can be chosen before a config is imported; choosing it shows the Import button (before, the option was greyed out until you found the button below it).
- Linux: on desktops without a keyring (Hyprland, Sway, i3, …) the app couldn't save the WARP account or VPN sign-ins, so it couldn't connect. It now keeps its encryption key in a file only you can read, and Settings says so. With KWallet or GNOME Keyring nothing changes, and a keyring that's locked or still starting at sign-in is waited for, never replaced by the file. Anything saved with the file while there was no keyring moves to the keyring once one is available.
- Linux: **Find Prowlarr or Jackett on this computer** now finds Prowlarr running as a system service (installed with its install script or the Arch/AUR package), not only one run from your own account.
- Linux and macOS: malware scans of finished downloads always failed ("ClamAV scan failed (exit code 2)") when ClamAV's background service wasn't running, which is the default on Arch, CachyOS and others. The app now runs ClamAV's standalone scanner, and scan errors show ClamAV's own message (for example, that its virus definitions haven't been downloaded yet).
- Linux and macOS: files over 100 MB showed as "no threats found" although ClamAV skips them by default. They now show as **couldn't scan fully**. For files up to 2 GB, a **Scan fully** button (in the files panel) scans them completely; ClamAV can't scan files over 2 GB, and stops after 2 GB of unpacked data (so a Linux ISO may still show as not fully scanned). A yellow ⚠ next to the torrent's name points this out until you've opened its files panel.

## [0.2.36] - 2026-10-19

### Fixed
- Removing some torrents (such as CachyOS's) closed the app. These torrents also list web mirrors, and the torrent library crashes when one is removed while it's using them. Mirrors (web seeds) are now turned off; downloads come from peers as usual.

## [0.2.35] - 2026-10-19

### Fixed
- The app could close right after opening when connected through a VPN without IPv6 (such as PIA) and a torrent listed an IPv6-only tracker. Those trackers are now skipped.

## [0.2.34] - 2026-10-17

### Fixed
- **Windows: the app failed to start on PCs without three MinGW files** ("libstdc++-6.dll was not found", also libgcc_s_seh-1.dll and libwinpthread-1.dll), including every Microsoft Store install. The Windows builds now need nothing beyond Windows itself, and each release build is checked for this.

## [0.2.33] - 2026-10-16

### Added
- **Remote control over Tailscale** (Settings → Remote control → "Also allow my Tailscale devices", off by default): use the phone remote away from home. With Tailscale on this computer and your phone, the remote also listens on this computer's Tailscale address, and only answers devices signed in to the same Tailscale account (checked with Tailscale for each device; tagged devices are never let in); Tailscale encrypts the traffic. Pair the phone over Tailscale and the same shortcut works at home and away. Paired phones are now tagged **Home Wi-Fi** or **Tailscale** in Settings.
- **Pause all / Resume all** on the phone remote (shown when there are two or more torrents).
- The phone remote says when a download finishes while it's open ("Finished: …"), with a short vibration where the phone supports it.
- The home-Wi-Fi remote only runs on the home network; until a phone has paired over home Wi-Fi, it only listens during that pairing.

### Security
- System tools the app runs (route, arp, Explorer, open, xdg-open, ClamAV) are started from their fixed system locations instead of being searched for on PATH.

## [0.2.32] - 2026-10-16

### Added
- **Remote control from your phone** (Settings → Remote control, off by default). Pair a phone by scanning a QR code, then use its browser on your home Wi-Fi to add magnet links or `.torrent` files, search, and pause or remove torrents on your computer. Downloads still happen on the computer, through its VPN. It only runs on the home network you paired on (it stays off on other Wi-Fi), only answers devices on that network, and you can remove a paired phone at any time. Add it to your phone's home screen for an app-like icon; pairing also works by typing a short code instead of scanning.

## [0.2.31] - 2026-10-16

### Added
- **Release notes in the app.** When an update is available, *What's new* in the update banner (and in Settings → About) lists every change since your version, right in the app. Settings → About also has *What's new* for the version you're running.

### Fixed
- **Privacy:** opening Settings or browsing VPN providers no longer contacts any provider over your normal connection. Found by an independent audit.
  - A provider's server list only loads once you've chosen *VPN account* and signed in to that provider.
  - While a VPN is connected, requests to VPN providers (server lists, sign-in, sign-out) go through it. That includes switching providers: the new provider is set up through the old VPN before it's closed, even if you switch several times in a row.
  - With no VPN connected (the first connection, after a failed connection, or in *No VPN* mode), providers are contacted directly only for what you do with them (signing in, connecting, signing out, or listing locations for one you're signed in to). These direct connections now close right away instead of lingering.
- **Privacy (Windows):** the app's window (Microsoft WebView2) no longer contacts Microsoft outside the VPN. Unpatched, at startup it looked up your Windows account at `substrate.office.com` and contacted `edge.microsoft.com`, did proxy auto-discovery (`wpad`), and when you typed in the app's boxes it called Microsoft's text prediction service. Those features are now off, and the window's own network layer is blocked (it never needs the network). Found by the new leak test.
- Error messages from PIA no longer include its temporary access token.
- Switching VPNs: if the new provider can't be set up through the current (working) VPN, the app now stays connected as before and shows **VPN switch failed** with a Retry button, instead of closing the VPN and contacting the new provider directly. If the current VPN has stopped working, the switch goes ahead as a first connection would, instead of getting stuck. Quick successive switches no longer interfere with each other or leave the status stuck on "Connecting…".

## [0.2.30] - 2026-10-16

### Security
- Releases are now checked before they're signed: every file must provably have been built by the project's own release workflow from the exact source code being released (GitHub build provenance). Files swapped into a release, or built from anything else, are refused. Thanks to the independent reviewer who spotted the gap.

### Changed
- The **No VPN** warning now mentions that, like other torrent clients, it may ask your router (UPnP / NAT-PMP) to open a port.

## [0.2.29] - 2026-10-16

### Added
- **Check it's working** (Settings → Search, under the proxy option): runs a test search and confirms from the actual traffic that Prowlarr or Jackett sends its searches through the VPN. Especially useful for Jackett, whose proxy you set up by hand.

## [0.2.28] - 2026-10-16

### Added
- **No VPN** option (Settings → VPN), for people whose router or computer already runs a VPN, or who only download legal content. It's never used automatically: if a VPN fails, downloads still stop. The header shows an amber **VPN off** while it's chosen.
- Search keeps working without the VPN. The Prowlarr/Jackett proxy refuses connections in this mode (so a proxied search server never switches to your real IP), and Settings explains how to let it search directly.

## [0.2.27] - 2026-10-16

### Added
- **Verify files** in the right-click menu: re-reads a torrent's files from disk and checks every piece against the torrent's hashes, catching damage after downloading (a bad copy, a failing drive or NAS). Bad pieces are downloaded again automatically.
- Torrents that include `.sfv` files are checked against them (CRC32 per file) when they finish, and again with Verify files.

## [0.2.26] - 2026-10-15

### Security
- **Signed releases.** Updates are signed with a key that never leaves the maintainer's computer, and the app refuses any update without a valid signature for that exact version, so even a hijacked GitHub account couldn't push an update the app would install. You can check a download yourself with `minisign` and `release.pub`.

## [0.2.25] - 2026-10-15

### Security
- Search downloads that point at an indexer site (instead of your own Prowlarr/Jackett) now go through the VPN, not your normal connection.
- The Windows uninstaller now removes only the files it installed. Before, uninstalling from a custom folder (like Downloads) could delete everything in that folder.
- Update downloads check the version format; the release workflow's actions are pinned to exact versions; library updates.

## [0.2.24] - 2026-10-15

### Added
- Microsoft Store packaging (the Store version gets its updates from the Store).
- A privacy policy (`PRIVACY.md`).

## [0.2.23] - 2026-10-15

### Added
- **Support development** button in Settings → About, now that GitHub Sponsors is live. Entirely optional; builds made with `-tags nofunding` don't have it.

## [0.2.22] - 2026-10-14

### Fixed
- Removing a selected torrent no longer shows a "torrent not found" error.

## [0.2.21] - 2026-10-14

### Fixed
- Show/Hide on the proxy password no longer collapses the proxy details.

## [0.2.20] - 2026-10-14

### Added
- Optional: send **Prowlarr's or Jackett's own traffic through the VPN** too (Settings → Search). The app sets Prowlarr up for you; Jackett shows the values to enter.

## [0.2.19] - 2026-10-14

### Fixed
- If Cloudflare drops the saved WARP device, the app registers a new one instead of failing until you delete its settings.
- PIA port forwarding renews itself before it expires, and its warning clears once it works again.
- Signing out of Mullvad removes the device from your account, so you don't run into Mullvad's 5-device limit.
- The Linux download now runs on older distributions (Debian, Ubuntu LTS) as well as Arch/CachyOS.
- macOS instructions updated for the current "Open Anyway" step.

## [0.2.18] - 2026-10-14

### Security
- Updated Go and two networking libraries to fix known vulnerabilities.

## [0.2.17] - 2026-10-14

### Fixed
- Downloading a search result from Prowlarr no longer fails with "The filename, directory name, or volume label syntax is incorrect".
- Error messages no longer show your Prowlarr/Jackett API key.

## [0.2.16] - 2026-10-14

### Changed
- Every column in the search results can be sorted (including Peers and Source).

## [0.2.15] - 2026-10-14

### Fixed
- Search results not appearing, with the button stuck on "Searching…".

## [0.2.14] - 2026-10-14

### Fixed
- Search couldn't reach Prowlarr/Jackett on this computer ("couldn't reach localhost") while the VPN was up.

## [0.2.13] - 2026-10-14

### Added
- **Search** through your own Prowlarr or Jackett, which the app can find on this computer automatically. No sites are built in.

## [0.2.12] - 2026-10-12

### Added
- **Malware checks**: torrents are checked for disguised programs as soon as their file list arrives, and finished downloads are marked as downloaded from the internet and scanned with Microsoft Defender (or ClamAV on Linux/macOS).

## [0.2.11] - 2026-10-12

### Fixed
- **Show files** on a single file whose path has spaces opened Documents instead of its folder.

## [0.2.10] - 2026-10-12

### Fixed
- Piece map flicker.

## [0.2.9] - 2026-10-12

### Fixed
- Piece map flashing once a second.

## [0.2.8] - 2026-10-12

### Fixed
- Updating could silently keep the old version installed.

## [0.2.7] - 2026-10-12

### Added
- **Piece map** in the torrent details: which pieces are done, downloading, being checked, missing or skipped.

## [0.2.6] - 2026-10-12

### Changed
- Much faster downloads to network drives (NAS).
- Download progress survives restarts.

### Fixed
- **Show files** for single-file torrents.

## [0.2.5] - 2026-10-12

### Changed
- Up to 200 peers per torrent (effectively unlimited); use the speed limits to control bandwidth.

## [0.2.4] - 2026-10-12

### Changed
- Connects to peers much faster.

### Added
- `-tags nofunding` build option that strips the sponsor and referral links.

## [0.2.3] - 2026-10-12

### Fixed
- Drag and drop of `.torrent` files; magnet links dragged from a browser are accepted.

## [0.2.2] - 2026-10-12

### Added
- Update checks and in-app updates.
- IPv6 trackers.
- A clear message when a private torrent can't start from a magnet link.

## [0.2.1] - 2026-10-12

### Fixed
- DHT and UDP trackers never working inside the VPN tunnel (much better peer discovery).

## [0.2.0] - 2026-10-12

First public release: a lightweight BitTorrent client with a built-in VPN that only the torrent client uses. Free Cloudflare WARP by default, or sign in to Mullvad, PIA (with port forwarding), NordVPN or IVPN, or import any WireGuard config. Windows, Linux and macOS.
