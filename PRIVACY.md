# Privacy policy

*Shark BiteTorrent, published by SharkByte Software. Last updated October 27, 2026.*

Shark BiteTorrent doesn't collect, store or share any information about you. There is no account with us, no analytics, no crash reporting and no ads, and the developers run no server for the app to send data to.

## What stays on your computer

Everything the app keeps is stored only on your own device:

- your settings and download list;
- your VPN sign-ins or imported WireGuard configuration (and, for PIA, the session token from your last sign-in, reused for up to 20 hours so the app doesn't sign in at every connection), encrypted with your operating system's protection (Windows DPAPI, or a key kept in the macOS Keychain or your Linux desktop's keyring). Like browsers' saved passwords, this keeps them unreadable on another computer or user account, but not from other programs running as you (on macOS any of your programs can read the Keychain entry without a prompt);
- the address and API key of your own Prowlarr or Jackett, if you connect one, and the phones you pair for remote control (also encrypted).

On a Linux desktop with no keyring (some tiling window managers), the encryption key is instead kept in a file in the same settings folder that only your user account can read, and Settings says so. In that case anyone who gets a copy of the whole folder (a backup, say) can decrypt it.

Uninstalling the app and deleting its settings folder removes all of it. If you turned on **Start when I sign in**, turn it off first: on macOS and Linux its sign-in entry lives outside the settings folder (the Windows uninstaller removes it for you).

## Who the app talks to

Normally everything below goes through the VPN. The exceptions are spelled out: what has to happen before a VPN exists, and what you choose with **No VPN**.

- **The BitTorrent network** (peers, trackers and DHT) for the torrents you add. Always through the VPN; if the VPN fails, downloads stop. Only if you choose *No VPN* does this use your normal connection, and then, like other torrent clients, the app may ask your router (UPnP / NAT-PMP) to open a port.
- **Your VPN provider:** Cloudflare WARP by default, or the provider you sign in to (Mullvad, Private Internet Access, NordVPN, IVPN), using that provider's own API. Their privacy policy covers what they see.
  - While a VPN is connected, requests to providers (server lists, sign-in, sign-out, setting up a switch to another provider) go through it. If setting up a switch through the current VPN fails while that VPN still works, the app stays on it and tells you, rather than contacting the new provider directly. If the current VPN has stopped working, there's no VPN to go through, so the switch continues as a first connection would.
  - With no VPN connected (the first connection, after a failed connection, or with *No VPN*), the app contacts providers directly only for what you do with them: signing in to or connecting to one (which sees your IP address when you connect anyway), signing out of one, or showing the server locations of one you're signed in to. Registering a free WARP account also goes direct: once at first launch, and again whenever connecting with the saved account fails (for example after Cloudflare drops it, or when the internet was down).
  - Opening Settings or browsing the list of providers doesn't contact any of them.
- **Your WireGuard config's server**, if you imported one whose server is a name rather than an address: the name is looked up with your system's DNS when the app first connects (when you switch to it while another VPN is up, it's looked up through that VPN instead).
- **Cloudflare's public trace page** (`cloudflare.com/cdn-cgi/trace`), through the VPN to check it works. When connecting to a provider other than WARP, it's also loaded once over your normal connection, to confirm the VPN's address differs from yours and to pick a server in your country.
- **GitHub**, to check for new versions: when the app starts (if it hasn't checked in the last hour) and about once a day while it runs, through the VPN, and you can turn this off in Settings. On the **Beta** update channel it also reads GitHub's list of recent releases (`github.com/…/releases.atom`) and checks that a beta is published before offering it, the same way. If you click *Check for updates* or *Update now* while the VPN is down, the app asks first and uses your normal connection only if you agree; with *No VPN* chosen, checks use your normal connection. When a new version is available, its release notes are also loaded from `raw.githubusercontent.com` the same way. The Microsoft Store version gets updates from the Store instead and doesn't check GitHub.
- **Your own Prowlarr or Jackett**, only if you set up search: directly if it's on this computer, your home network or Tailscale (the VPN can't reach those), and through the VPN anywhere else, such as a seedbox (whose name is then never looked up outside the VPN either). Downloading a search result goes through your Prowlarr/Jackett, or through the VPN for links to other sites (directly with *No VPN*). If you turn on the search proxy, the app changes Prowlarr's proxy setting through Prowlarr's API, and changes it back when you turn the proxy off.

**Your operating system's own protection** works outside the app, over your normal connection: when the app marks finished downloads as coming from the internet and scans them (on by default; Settings → Desktop), Windows Defender, SmartScreen or macOS Gatekeeper may check the files with Microsoft or Apple, as they do for anything you download. ClamAV on Linux and macOS checks files locally.

**Certificates are checked inside the app.** When the app connects to a site over HTTPS (a VPN provider's sign-in, an update, a tracker), it checks the site's certificate itself, against the trusted root certificates (Mozilla's list) built into the app. It doesn't ask your operating system, whose certificate checker on macOS and Windows can go online by itself, over your normal connection, to fetch revocation information or a missing certificate, which would tell your network which sites the app talks to. The costs: certificate authorities you added to your system yourself (a company proxy's, say) aren't trusted by the app; a site whose server doesn't send its full certificate chain fails instead of the system fetching the missing part; revoked certificates aren't detected (as with Go programs on Linux); and the list of trusted roots is as new as your version of the app, so keep it updated.

On Windows, the app's window is drawn by Microsoft WebView2 (the engine of Microsoft Edge), which on its own would contact Microsoft: looking up your Windows account and signing in to it, text prediction and Microsoft Editor (which send what you type), proxy auto-discovery, Edge configuration and component updates. The app turns those features off and blocks the window's own network layer (it never needs the network). The release leak test checks the app and every process it starts; Windows' own sign-in broker runs separately, so the test reports its activity rather than counting it. Windows may also update the WebView2 runtime through its own update service, outside the app. On Linux the window uses WebKitGTK; the Linux leak test covers it and every helper process it starts, and found no network traffic from it. On macOS the window uses the system's web view (WebKit); the macOS leak test covers the app's WebKit processes and found no network traffic from them. On every platform the window's content is restricted to the app itself (a Content-Security-Policy), so it can't load anything from the internet.

**Remote control** (off unless you turn it on in Settings): the app then also accepts connections from devices on your local network, on the port shown in Settings (8732 by default). It only listens on real local networks (Wi-Fi or Ethernet with a router), and only on the home network: the one a phone first paired on over home Wi-Fi (until then, only while you're pairing one). On other Wi-Fi (cafés, hotels, work) that part stays off. It only answers private network addresses and never accepts connections through this app's VPN. Paired phones get a random access key, stored on this computer only as a hash, and the phone page is only sent what it shows (not your VPN's address, forwarded port or download folders). The page uses plain HTTP, so someone else on your Wi-Fi could see torrent names, searches, magnet links and the phone's access key (which can remove torrents and their files). If you also turn on **Tailscale access**, it listens on this computer's Tailscale address too, wherever the computer is (including away from home), and only answers devices that Tailscale confirms are signed in to the same Tailscale account as this computer. That traffic is encrypted by Tailscale, sometimes through Tailscale's own relay servers; Tailscale's app on your computer handles the connection, outside this app's VPN.

None of these connections carry information about you to the developers.

## Links in the app

Settings has optional links to the project's GitHub Sponsors page and to VPN providers' sign-up pages, some of which are marked as referral links. They only open in your browser when you click them, and the app doesn't track whether you do.

## Children

The app doesn't knowingly collect information from anyone, including children.

## Changes and contact

Changes to this policy are made in this file, and its history is public on GitHub. Questions: open an issue at <https://github.com/trevercomb/SharkBiteTorrent/issues>.
