<script lang="ts" module>
  // The tab Settings opens on: the last one used, while the app runs.
  let lastTab = 'vpn'
</script>

<script lang="ts">
  import InstallCommand from './InstallCommand.svelte'
  import {
    GetSettings, SaveSettings, ChooseDirectory,
    RegisterAssociations, OpenDefaultAppsSettings, GetAbout, OpenURL,
    CheckForUpdates, InstallUpdate, DownloadUpdate, GetReleaseNotes,
    GetStartup, SetStartup, OpenStartupSettings,
  } from '../wailsjs/go/main/App.js'
  import { main, type platform } from '../wailsjs/go/models'
  import { Environment } from '../wailsjs/runtime/runtime.js'
  import VpnSettings from './VpnSettings.svelte'
  import SearchSettings from './SearchSettings.svelte'
  import RemoteSettings from './RemoteSettings.svelte'
  import ReleaseNotes from './ReleaseNotes.svelte'

  let { onClose }: { onClose: () => void } = $props()

  let s = $state<main.Settings | null>(null)
  let original = $state<main.Settings | null>(null)
  let endpoint = $state('')
  let accounts = $state<Record<string, string>>({})
  let assoc = $state<platform.AssocStatus>({ supported: false, magnet: false, torrent: false, canOpenSettings: false })
  let scanner = $state("")
  let scannerAvailable = $state(false)
  let keyringMissing = $state(false)
  let error = $state('')
  let saving = $state(false)
  let about = $state<main.About | null>(null)
  const providerNames: Record<string, string> = {
    ivpn: 'IVPN', mullvad: 'Mullvad', nordvpn: 'NordVPN', pia: 'Private Internet Access',
  }
  const referralIds = $derived(
    Object.entries(about?.links.providers ?? {}).filter(([, p]) => p.referral).map(([id]) => id).sort(),
  )
  const hasReferrals = $derived(referralIds.length > 0)
  const referralNames = $derived(referralIds.map(id => providerNames[id] ?? id).join(' and '))

  GetAbout().then(a => (about = a))

  // macOS has no tray icon: closing always hides the app (see tray_darwin.go).
  let isMac = $state(false)
  Environment().then(e => (isMac = e.platform === 'darwin'))

  // "Start when I sign in" lives in the OS (registry / autostart file), so
  // it's read from there and applied on Save like everything else.
  let startup = $state<main.StartupStatus | null>(null)
  let startupOn = $state(false)
  GetStartup().then(v => {
    startup = v
    startupOn = v.enabled
  })
  let currentNotes = $state<main.ReleaseNote[]>([])
  let showCurrentNotes = $state(false)
  GetReleaseNotes().then(n => (currentNotes = n ?? []))

  let checking = $state(false)
  let installing = $state(false)
  let updateResult = $state<main.UpdateInfo | null>(null)
  // Set when the VPN is down: the user is asked before anything uses the
  // normal connection.
  let askDirect = $state<'check' | 'install' | 'download' | null>(null)
  let downloaded = $state<main.DownloadedUpdate | null>(null)
  const vpnDown = (e: unknown) => String(e).includes("VPN isn't connected")

  async function checkUpdates(direct = false) {
    checking = true
    askDirect = null
    updateResult = null
    error = ''
    try {
      updateResult = await CheckForUpdates(direct)
    } catch (e) {
      if (vpnDown(e)) askDirect = 'check'
      else error = String(e)
    }
    checking = false
  }

  async function downloadUpdate(direct = false) {
    installing = true
    askDirect = null
    error = ''
    try {
      downloaded = await DownloadUpdate(updateResult!.latest, direct)
    } catch (e) {
      if (vpnDown(e)) askDirect = 'download'
      else error = String(e)
    }
    installing = false
  }

  async function installUpdate(direct = false) {
    installing = true
    askDirect = null
    error = ''
    try {
      await InstallUpdate(updateResult!.latest, direct) // the app quits and relaunches
    } catch (e) {
      if (vpnDown(e)) askDirect = 'install'
      else error = String(e)
    }
    installing = false
  }

  GetSettings().then(v => {
    s = { ...v.settings } as main.Settings
    original = { ...v.settings } as main.Settings
    endpoint = v.customEndpoint
    accounts = v.accounts ?? {}
    assoc = v.assoc
    scanner = v.scanner
    scannerAvailable = v.scannerAvailable
    keyringMissing = v.keyringMissing
  })

  const vpnChanged = $derived(
    !!s && !!original &&
      (s.vpnProvider !== original.vpnProvider ||
        (s.vpnProvider === 'custom' && s.forwardedPort !== original.forwardedPort) ||
        (s.vpnProvider === 'account' &&
          (s.accountProvider !== original.accountProvider || s.accountRegion !== original.accountRegion))),
  )

  async function attempt(fn: () => Promise<unknown>) {
    error = ''
    try {
      await fn()
    } catch (e) {
      error = String(e)
    }
  }

  const pickDir = () =>
    attempt(async () => {
      const d = await ChooseDirectory(s!.downloadDir)
      if (d) s!.downloadDir = d
    })

  const register = () => attempt(async () => (assoc = await RegisterAssociations()))

  async function save() {
    saving = true
    await attempt(async () => {
      await SaveSettings(main.Settings.createFrom({
        ...s!,
        forwardedPort: Number(s!.forwardedPort) || 0,
        downLimitKB: Number(s!.downLimitKB) || 0,
        upLimitKB: Number(s!.upLimitKB) || 0,
      }))
      // After the settings saved, so a refused save doesn't half-apply.
      if (startup && !startup.viaWindowsSettings && startupOn !== startup.enabled) {
        await SetStartup(startupOn)
        startup.enabled = startupOn
      }
      onClose()
    })
    saving = false
  }

  // Tabs: every section stays mounted (only hidden), so edits on one tab
  // survive switching, and VPN/Search/Remote keep their own state.
  const tabs = [
    { id: 'vpn', label: 'VPN' },
    { id: 'downloads', label: 'Downloads' },
    { id: 'search', label: 'Search' },
    { id: 'remote', label: 'Remote' },
    { id: 'app', label: 'App' },
  ]
  let tab = $state(lastTab)
  $effect(() => { lastTab = tab })
  // Unsaved changes per tab (Search and Remote apply theirs at once).
  const changed = $derived.by((): Record<string, boolean> => {
    if (!s || !original) return {}
    const diff = (...keys: (keyof main.Settings)[]) => keys.some(k => String(s![k]) !== String(original![k]))
    return {
      vpn: vpnChanged,
      downloads: diff('downloadDir', 'askSaveLocation', 'downLimitKB', 'upLimitKB', 'scanDownloads', 'autoVerify'),
      app: diff('closeToTray', 'autoUpdateCheck', 'updateChannel') || (!!startup && !startup.viaWindowsSettings && startupOn !== startup.enabled),
    }
  })

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape') onClose()
  }
</script>

<svelte:window onkeydown={onKey} />

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
<div class="backdrop" onclick={onClose}>
  <div class="dialog" role="dialog" aria-modal="true" aria-labelledby="settings-title" tabindex="-1" onclick={e => e.stopPropagation()}>
    <h2 id="settings-title">Settings</h2>

    {#if !s}
      <p class="muted">Loading…</p>
    {:else}
      <div class="tabs" role="tablist">
        {#each tabs as t}
          <button role="tab" class="tab" class:active={tab === t.id} aria-selected={tab === t.id} onclick={() => (tab = t.id)}>
            {t.label}{#if changed[t.id]}<span class="dot" title="Unsaved changes"></span>{/if}
          </button>
        {/each}
      </div>
      <div class="body">
        <div class="panel" role="tabpanel" hidden={tab !== 'vpn'}>
          <fieldset>
            <VpnSettings {s} bind:endpoint bind:accounts onError={e => (error = e)} />
            {#if vpnChanged}<p class="note">Saving will reconnect the VPN. Torrents pick up where they left off.</p>{/if}
            {#if keyringMissing}
              <p class="muted small">
                No system keyring (KWallet, GNOME Keyring or another Secret Service) is available, so saved
                secrets (VPN keys and sign-ins, the search API key, paired phones) are encrypted with a key file
                only your user can read. Start a keyring for stronger protection.
              </p>
            {/if}
          </fieldset>
        </div>
        <div class="panel" role="tabpanel" hidden={tab !== 'downloads'}>
          <fieldset>
            <legend>Downloads</legend>
            <div class="row">
              <input class="grow" readonly value={s.downloadDir} title={s.downloadDir} />
              <button onclick={pickDir}>Change…</button>
            </div>
            <p class="muted small">Used for torrents you add from now on. Existing torrents stay where they are.</p>
            <label class="check">
              <input type="checkbox" bind:checked={s.askSaveLocation} />
              <span>Ask where to save each download</span>
            </label>
            <p class="muted small">
              Before anything is added, choose its folder (this one, a recent one, or another) and whether to start it now.
            </p>
          </fieldset>

          <fieldset>
            <legend>Speed limits</legend>
            <label class="row">
              <span class="lbl">Download</span>
              <input type="number" min="0" bind:value={s.downLimitKB} class="short" /> <span class="muted">KB/s</span>
            </label>
            <label class="row">
              <span class="lbl">Upload</span>
              <input type="number" min="0" bind:value={s.upLimitKB} class="short" /> <span class="muted">KB/s</span>
            </label>
            <p class="muted small">0 = unlimited. Changes apply immediately.</p>
          </fieldset>

          <fieldset>
            <legend>Checking downloads</legend>
            <label class="check">
              <input type="checkbox" bind:checked={s.scanDownloads} disabled={!scannerAvailable} />
              <span>
                Scan finished downloads for malware with {scanner}
                {#if !scannerAvailable}<span class="muted">(not installed)</span>{/if}
              </span>
            </label>
            {#if !scannerAvailable && scanner === 'ClamAV'}
              <p class="muted small">
                Install ClamAV to enable scanning: <code>sudo pacman -S clamav</code> on Arch/CachyOS,
                <code>brew install clamav</code> on macOS. Torrents are still checked for disguised programs either way.
              </p>
            {:else}
              <p class="muted small">
                Every torrent is also checked for disguised programs as soon as its file list arrives, and finished
                files are marked as downloaded from the internet so the system checks them when opened.
              </p>
            {/if}
            <label class="row">
              <span class="lbl">Auto verify</span>
              <select bind:value={s.autoVerify}>
                <option value="off">Off</option>
                <option value="suspect">Suspect downloads only</option>
                <option value="all">All downloads</option>
              </select>
            </label>
            <p class="muted small">
              Re-checks a finished download's files against the torrent before the malware scan, and downloads any
              damaged pieces again. Every piece is already checked when it arrives and confirmed to be on the drive
              before it counts, so this matters when something could have gone wrong afterwards.
              <b>Suspect downloads only</b> (the default) verifies a download if its drive disconnected, a read or
              write failed, or the app didn't shut down normally while it was downloading.
              <b>All downloads</b> also verifies everything else: it reads every finished download back from the
              drive once more, which takes a while for large files and adds load on a network drive (around 10
              minutes for a 50 GB file on a typical NAS), and it almost never finds anything the default wouldn't.
              You can always run <b>Verify files</b> on a torrent yourself.
            </p>
          </fieldset>
        </div>
        <div class="panel" role="tabpanel" hidden={tab !== 'search'}>
          <fieldset>
            <p class="note-now">Changes on this tab take effect right away; Save and Cancel don't affect them.</p>
            <SearchSettings onError={e => (error = e)} />
          </fieldset>
        </div>
        <div class="panel" role="tabpanel" hidden={tab !== 'remote'}>
          <fieldset>
            <p class="note-now">Changes on this tab take effect right away; Save and Cancel don't affect them.</p>
            <RemoteSettings onError={e => (error = e)} />
          </fieldset>
        </div>
        <div class="panel" role="tabpanel" hidden={tab !== 'app'}>
          <fieldset>
            <legend>Desktop</legend>
            {#if startup?.viaWindowsSettings}
              <p class="muted small">
                To start Shark BiteTorrent when you sign in, turn it on in
                <button class="link" onclick={() => OpenStartupSettings()}>Windows Settings → Apps → Startup</button>.
              </p>
            {:else if startup}
              <label class="check">
                <input type="checkbox" bind:checked={startupOn} />
                <span>Start Shark BiteTorrent in the background when I sign in</span>
              </label>
            {/if}
            {#if isMac}
              <p class="muted small">
                Closing the window keeps Shark BiteTorrent running. Click its Dock icon to bring it back, or press Cmd+Q to quit.
              </p>
            {:else}
              <label class="check">
                <input type="checkbox" bind:checked={s.closeToTray} />
                <span>Closing the window keeps Shark BiteTorrent running in the tray</span>
              </label>
            {/if}
            {#if about?.store}
              <p class="muted small">
                Installed from the Microsoft Store: magnet links and .torrent files were registered when it was installed.
                To make Shark BiteTorrent the app that opens them,
                <button class="link" onclick={() => OpenDefaultAppsSettings()}>open Default Apps</button>.
              </p>
            {:else if assoc.supported}
              <div class="row">
                <span class="status">
                  Magnet links: <b class:ok={assoc.magnet}>{assoc.magnet ? 'open here' : 'not set'}</b> ·
                  .torrent files: <b class:ok={assoc.torrent}>{assoc.torrent ? 'open here' : 'not set'}</b>
                </span>
              </div>
              <div class="row">
                <button onclick={register}>Open magnet links &amp; .torrent files with Shark BiteTorrent</button>
              </div>
              {#if !(assoc.magnet && assoc.torrent) && assoc.canOpenSettings}
                <p class="muted small">
                  If you picked another app before, your system keeps using it.
                  <button class="link" onclick={() => OpenDefaultAppsSettings()}>Open Default Apps</button>
                  and pick Shark BiteTorrent there.
                </p>
              {/if}
            {/if}
          </fieldset>

          {#if about}
            <fieldset>
              <legend>About</legend>
              <div class="row">
                <span>Shark BiteTorrent {about.version}</span>
                <span class="muted">· free software ({about.license})</span>
                {#if currentNotes.length > 0}
                  <button class="link" onclick={() => (showCurrentNotes = !showCurrentNotes)}>
                    {showCurrentNotes ? 'Hide' : "What's new"}
                  </button>
                {/if}
                <button class="link" onclick={() => OpenURL(about!.links.repo)}>Source code ↗</button>
              </div>
              {#if showCurrentNotes}
                <div class="notes-box"><ReleaseNotes notes={currentNotes} /></div>
              {/if}
              {#if about.store}
                <p class="muted small">Updates are installed by the Microsoft Store.</p>
              {:else}
              <div class="row">
                <button onclick={() => checkUpdates()} disabled={checking || installing}>
                  {checking ? 'Checking…' : 'Check for updates'}
                </button>
                {#if updateResult}
                  {#if updateResult.available}
                    <span>Version <b>{updateResult.latest}</b> is available.</span>
                    {#if updateResult.canInstall}
                      <button class="primary" onclick={() => installUpdate()} disabled={installing}>
                        {installing ? 'Downloading…' : 'Update now'}
                      </button>
                    {:else if updateResult.canDownload}
                      <button class="primary" onclick={() => downloadUpdate()} disabled={installing}>
                        {installing ? 'Downloading…' : 'Download (verified)'}
                      </button>
                    {:else}
                      <button class="primary" onclick={() => OpenURL(updateResult!.notesURL)}>Download</button>
                    {/if}
                    <button class="link" onclick={() => OpenURL(updateResult!.notesURL)}>Release page ↗</button>
                  {:else}
                    <span class="ok">You're up to date.</span>
                  {/if}
                {/if}
              </div>
              {#if updateResult?.available && (updateResult.notes ?? []).length > 0}
                <div class="notes-box"><ReleaseNotes notes={updateResult.notes} /></div>
              {/if}
              {#if downloaded}
                <InstallCommand path={downloaded.path} hint={downloaded.hint} command={downloaded.command} />
              {/if}
              {#if askDirect}
                <div class="row">
                  <span>The VPN isn't connected. {askDirect === 'check' ? 'Check' : 'Download the update'} over your normal connection instead? GitHub will see your real IP address.</span>
                  <button onclick={() => (askDirect === 'check' ? checkUpdates(true) : askDirect === 'download' ? downloadUpdate(true) : installUpdate(true))}>Use normal connection</button>
                  <button onclick={() => (askDirect = null)}>Cancel</button>
                </div>
              {/if}
              {#if updateResult?.direct}
                <p class="muted small">The VPN was off, so this check used your normal connection.</p>
              {/if}
              <label class="check">
                <input type="checkbox" bind:checked={s.autoUpdateCheck} />
                <span>Check for updates automatically <span class="muted">(when it starts and about once a day, through the VPN)</span></span>
              </label>
              <label class="row">
                <span class="lbl">Update channel</span>
                <select bind:value={s.updateChannel}>
                  <option value="stable">Stable</option>
                  <option value="beta">Beta</option>
                </select>
              </label>
              <p class="muted small">
                <b>Beta</b> also offers previews of the next version, so you get fixes and new features sooner and can
                help find problems before a release. Betas are signed and checked like releases, but have had less
                testing. Switching back to <b>Stable</b> keeps the beta you have; you're offered the next release
                once it's out. To go back to an older version, install it from the releases page. Save, then check
                for updates to see the change.
              </p>
              {/if}
              {#if about.links.sponsor}
                <div class="row">
                  <button class="sponsor" onclick={() => OpenURL(about!.links.sponsor)}>♥ Support development</button>
                  <span class="muted small">No ads, no tracking. Donations keep it that way.</span>
                </div>
              {/if}
              {#if !about.funding}
              <p class="muted small">
                <b>This build has funding links turned off</b> (built with <code>-tags nofunding</code>): no sponsor
                button and no referral links. No ads, no telemetry: the app never sends anything to the developers.
                Updates are offered as a download rather than installed, because official releases include the links.
              </p>
              {:else}
              <p class="muted small">
                <b>How this app is funded:</b> optional donations{#if hasReferrals}, and referral links for
                  {referralNames}: if you subscribe through one, the provider pays this project a commission
                  (same price for you; the provider's site will know you came from this app){/if}.
                Providers are listed alphabetically, never by commission. Free WARP and WireGuard import are always available.
                No ads, no telemetry: the app never sends anything to us.
                <button class="link" onclick={() => OpenURL(about!.links.repo + '#how-this-project-is-funded')}>Details ↗</button>
              </p>
              {/if}
            </fieldset>
          {/if}
        </div>
      </div>
    {/if}

    {#if error}<div class="error" role="alert">{error}</div>{/if}

    <div class="actions">
      <button onclick={onClose}>Cancel</button>
      <button class="primary" onclick={save} disabled={!s || saving}>Save</button>
    </div>
  </div>
</div>

<style>
  .backdrop {
    position: fixed; inset: 0; background: rgba(0, 0, 0, 0.55);
    display: grid; place-items: center; z-index: 10;
  }
  .dialog {
    width: min(640px, calc(100vw - 32px)); max-height: calc(100vh - 32px);
    display: flex; flex-direction: column;
    background: var(--panel); border: 1px solid var(--border); border-radius: 10px;
    box-shadow: 0 20px 50px rgba(0, 0, 0, 0.5);
  }
  h2 { margin: 0; padding: 16px 20px 8px; font-size: 18px; font-weight: 600; }
  .tabs { display: flex; gap: 2px; padding: 0 20px; border-bottom: 1px solid var(--border); }
  .tab {
    background: none; border: none; border-bottom: 2px solid transparent; border-radius: 0;
    padding: 8px 12px; color: var(--muted); display: inline-flex; align-items: center; gap: 6px;
  }
  .tab:hover { color: inherit; background: none !important; }
  .tab.active { color: inherit; border-bottom-color: var(--accent); }
  .dot { width: 6px; height: 6px; border-radius: 50%; background: var(--warn); }
  .body { overflow: auto; padding: 0 20px; height: min(560px, calc(100vh - 200px)); }
  .panel > fieldset:first-child { border-top: none; }
  .panel[hidden] { display: none !important; }
  .note-now { color: var(--muted); font-size: 12px; margin: 0 0 4px; }
  fieldset { border: none; border-top: 1px solid var(--border); margin: 0; padding: 12px 0 14px; }
  legend { color: var(--muted); font-size: 12px; text-transform: uppercase; letter-spacing: 0.06em; padding-right: 8px; }
  .row { display: flex; align-items: center; gap: 8px; margin-top: 8px; flex-wrap: wrap; }
  .check { display: flex; align-items: flex-start; gap: 10px; margin-top: 8px; cursor: pointer; }
  .check input { margin-top: 3px; }
  .lbl { min-width: 72px; white-space: nowrap; }
  .short { width: 100px; }
  .grow { flex: 1; min-width: 0; }
  .muted { color: var(--muted); }
  .small { font-size: 12px; margin: 6px 0 0; }
  .note { color: var(--warn); margin: 10px 0 0; font-size: 13px; }
  .status b { font-weight: 600; color: var(--warn); }
  .status b.ok { color: var(--ok); }
  .ok { color: var(--ok); }
  .link { background: none; border: none; padding: 0; color: var(--accent); text-decoration: underline; }
  .sponsor { border-color: #db61a2; color: #f0a6cf; }
  .sponsor:hover { background: rgba(219, 97, 162, 0.15) !important; }
  .error {
    margin: 8px 20px 0; padding: 8px 12px; border-radius: 6px; user-select: text;
    background: rgba(248, 81, 73, 0.12); border: 1px solid rgba(248, 81, 73, 0.4);
  }
  .actions { display: flex; justify-content: flex-end; gap: 8px; padding: 14px 20px; border-top: 1px solid var(--border); }
  .notes-box { margin: 8px 0 4px; padding: 8px 10px; border: 1px solid var(--border); border-radius: 6px; }
</style>
