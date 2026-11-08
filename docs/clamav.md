# Setting up ClamAV (Linux and macOS)

On Windows, Shark BiteTorrent scans finished downloads with Microsoft Defender, which is already there. On Linux and macOS it uses [ClamAV](https://www.clamav.net/), a free antivirus you install yourself. Installing it isn't quite enough: ClamAV also needs its list of known viruses (its *signature database*), and some systems don't download that for you. Without it, every scan fails.

Once ClamAV is set up, turn on **Settings → Desktop → Scan finished downloads for malware with ClamAV** in the app. If it says *(not installed)*, close Settings and open it again.

## Arch, CachyOS, Manjaro

```sh
sudo pacman -S clamav
sudo freshclam                                         # download the virus list now
sudo systemctl enable --now clamav-freshclam.service   # keep it up to date
```

You don't need `clamav-daemon` (clamd): the app runs `clamscan` itself.

## Debian, Ubuntu, Linux Mint

```sh
sudo apt install clamav
```

The package downloads the virus list and keeps it updated on its own (the `clamav-freshclam` service). The first download can take a few minutes after installing.

## Fedora

```sh
sudo dnf install clamav clamav-update
sudo freshclam
```

## Other Linux

Install ClamAV from your distribution, then run `sudo freshclam` once. If freshclam says its config file is an example, see the macOS steps below for the fix.

## macOS (Homebrew)

```sh
brew install clamav
cd /opt/homebrew/etc/clamav            # /usr/local/etc/clamav on Intel Macs
sed 's/^Example/#Example/' freshclam.conf.sample > freshclam.conf
echo "CVDCertsDirectory $PWD/certs" >> freshclam.conf
freshclam                              # download the virus list (about 110 MB)
```

Homebrew installs a sample config whose first setting, `Example`, stops freshclam from running until it's switched off; the `sed` line makes a working copy with it commented out. ClamAV 1.5 and later also check the virus list's signature against the certificate in `certs`, and the sample looks for it in `/etc/clamav/certs`, where Homebrew doesn't put it: the `echo` line points it to Homebrew's copy (without it, freshclam says `NULL X509 store`). No `sudo` is needed: Homebrew's folders belong to you.

Run `freshclam` again now and then (weekly, say) to get new virus signatures.

### Or ClamAV's own installer

The `.pkg` from [clamav.net](https://www.clamav.net/downloads) installs ClamAV in `/usr/local/clamav` (the app finds it there), but without a working config or a virus list. Finish it in Terminal (each line asks for your password):

```sh
sudo sh -c "sed 's/^Example/#Example/' /usr/local/clamav/etc/freshclam.conf.sample > /usr/local/clamav/etc/freshclam.conf && echo 'CVDCertsDirectory /usr/local/clamav/etc/certs' >> /usr/local/clamav/etc/freshclam.conf"
sudo mkdir -p /usr/local/clamav/share/clamav && sudo chown _clamav:_clamav /usr/local/clamav/share/clamav
sudo /usr/local/clamav/bin/freshclam   # download the virus list (about 110 MB)
```

The folders belong to the system here, hence `sudo`; freshclam hands the virus list to macOS's built-in `_clamav` account. Run the last line again now and then to get new virus signatures.

## Checking it works

[EICAR](https://www.eicar.org/download-anti-malware-testfile/) is a harmless test file that every antivirus reports. Paste this into a terminal:

```sh
printf '%s' 'X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*' > /tmp/eicar.com
clamscan /tmp/eicar.com; rm /tmp/eicar.com
```

It should say `Eicar-Test-Signature FOUND`.

## What the app does with big files

- Files over 100 MB are skipped by the automatic scan (it would take too long), and the app says so instead of calling them clean. **Scan fully** in the torrent's files panel scans them.
- ClamAV can't scan files over 2 GB at all, and stops after 2 GB of data, counting what it unpacks from inside a file. A Linux ISO usually unpacks to more than that, so even a full scan of one stops partway; the app tells you when that happens.

## If scans fail

The app shows ClamAV's own error. The usual one is `No supported database files found`: the virus list was never downloaded, so run `freshclam` (with `sudo` on Linux) as above.
