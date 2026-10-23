<script lang="ts">
  import InstallCommand from './InstallCommand.svelte'
  import { onMount } from 'svelte'
  import {
    GetVpnStatus, ListTorrents, AddMagnet, PickTorrentFile, GetSettings,
    Pause, Resume, Remove, Reconnect, OpenFolder, ChooseDownloadDir,
    InstallUpdate, DownloadUpdate, SkipUpdate, OpenURL, VerifyTorrent, StartTorrent,
  } from '../wailsjs/go/main/App.js'
  import { EventsOn, OnFileDrop } from '../wailsjs/runtime/runtime.js'
  import type { engine, main } from '../wailsjs/go/models'
  import FilesPanel from './FilesPanel.svelte'
  import ReleaseNotes from './ReleaseNotes.svelte'
  import SearchView from './SearchView.svelte'
  import Settings from './Settings.svelte'
  import { bytes, rate, duration } from './format'

  let vpn = $state<main.VpnStatus>({ state: 'connecting' } as main.VpnStatus)
  let torrents = $state<engine.Status[]>([])
  let selected = $state<string | null>(null)
  let magnet = $state('')
  let error = $state('')
  let notice = $state('') // worth seeing, not an error (e.g. a phone paired)
  let settings = $state<main.Settings | null>(null)
  let showSettings = $state(false)
  let searching = $state(false) // the search view replaces the torrent list
  let removing = $state<engine.Status | null>(null)
  let deleteFiles = $state(false)

  // "off" is the user's explicit No VPN choice; the engine runs either way.
  const connected = $derived(vpn.state === 'connected' || vpn.state === 'off')
  const sel = $derived(torrents.find(t => t.hash === selected))
  const totalDown = $derived(torrents.reduce((s, t) => s + t.downRate, 0))
  const totalUp = $derived(torrents.reduce((s, t) => s + t.upRate, 0))

  async function refresh() {
    try {
      vpn = await GetVpnStatus()
      torrents = await ListTorrents()
      if (selected && !torrents.some(t => t.hash === selected)) selected = null
    } catch (e) {
      error = String(e)
    }
  }

  const loadSettings = async () => (settings = (await GetSettings()).settings)

  onMount(() => {
    loadSettings()
    refresh()
    const id = setInterval(refresh, 1000)
    const off = EventsOn('error', (msg: string) => (error = msg))
    const offUpdate = EventsOn('update-available', (info: main.UpdateInfo) => (update = info))
    const offNotice = EventsOn('notice', (msg: string) => (notice = msg))
    // Wails only attaches its window drop listeners when OnFileDrop is called
    // from JS. Dropped file paths are then handled in Go (runtime.OnFileDrop in
    // app.go), so the JS callback has nothing to do. useDropTarget=false: the
    // whole window accepts drops, including the magnet input.
    OnFileDrop(() => {}, false)
    window.addEventListener('dragenter', onDragEnter)
    window.addEventListener('dragover', onDragOverText)
    window.addEventListener('dragleave', onDragLeaveWindow)
    window.addEventListener('drop', onDropText)
    return () => {
      window.removeEventListener('dragenter', onDragEnter)
      window.removeEventListener('dragover', onDragOverText)
      window.removeEventListener('dragleave', onDragLeaveWindow)
      window.removeEventListener('drop', onDropText)
      clearInterval(id)
      off()
      offUpdate()
      offNotice()
    }
  })

  async function run(fn: () => Promise<unknown>) {
    error = ''
    try {
      await fn()
      await refresh()
    } catch (e) {
      error = String(e)
    }
  }

  // Adding or starting a torrent creates its files, which on a busy NAS can
  // take a minute: show that it's happening and don't take the click twice.
  let adding = $state(0)
  async function runAdding(fn: () => Promise<unknown>) {
    adding++
    try {
      await run(fn)
    } finally {
      adding--
    }
  }

  function addMagnet(ev: SubmitEvent) {
    ev.preventDefault()
    if (!magnet.trim()) return
    runAdding(async () => {
      await AddMagnet(magnet)
      magnet = ''
    })
  }

  // Drag and drop. Files (.torrent) are handled by Wails + Go; magnet links
  // dragged from a browser arrive as text and are handled here.
  let dragging = $state(false)
  const isDroppable = (e: DragEvent) =>
    !!e.dataTransfer && ['Files', 'text/uri-list', 'text/plain'].some(t => e.dataTransfer!.types.includes(t))

  function onDragEnter(e: DragEvent) {
    if (isDroppable(e)) dragging = true
  }
  function onDragOverText(e: DragEvent) {
    // Allow text (magnet) drops anywhere; Wails already handles file drags.
    if (e.dataTransfer && !e.dataTransfer.types.includes('Files') && isDroppable(e)) e.preventDefault()
  }
  function onDragLeaveWindow(e: DragEvent) {
    if (!e.relatedTarget) dragging = false // left the window, not just a child element
  }
  function onDropText(e: DragEvent) {
    dragging = false
    const dt = e.dataTransfer
    if (!dt || dt.types.includes('Files')) return
    const text = (dt.getData('text/uri-list') || dt.getData('text/plain')).trim()
    const links = text.split(/\s+/).filter(l => l.startsWith('magnet:'))
    if (links.length === 0) return
    e.preventDefault()
    run(async () => {
      for (const l of links) await AddMagnet(l)
    })
  }

  // Update offered by the launch-time check (or Settings → Check for updates).
  let update = $state<main.UpdateInfo | null>(null)
  let showNotes = $state(false)
  let installing = $state(false)
  let askDirect = $state<'install' | 'download' | null>(null) // VPN down: ask before using the normal connection
  let downloaded = $state<main.DownloadedUpdate | null>(null)

  async function downloadUpdate(direct = false) {
    installing = true
    askDirect = null
    try {
      downloaded = await DownloadUpdate(update!.latest, direct)
    } catch (e) {
      if (String(e).includes("VPN isn't connected")) askDirect = 'download'
      else error = String(e)
    }
    installing = false
  }

  async function installUpdate(direct = false) {
    installing = true
    askDirect = null
    try {
      await InstallUpdate(update!.latest, direct) // the app quits, and the new version starts
    } catch (e) {
      if (String(e).includes("VPN isn't connected")) askDirect = 'install'
      else error = String(e)
      installing = false
    }
  }

  // Right-click menu on a torrent row.
  let menu = $state<{ x: number; y: number; t: engine.Status } | null>(null)

  function openMenu(e: MouseEvent, t: engine.Status) {
    e.preventDefault()
    selected = t.hash
    // Keep the menu inside the window.
    const w = 170, h = 120
    menu = {
      x: Math.min(e.clientX, window.innerWidth - w - 4),
      y: Math.min(e.clientY, window.innerHeight - h - 4),
      t,
    }
  }

  function menuAction(fn: (t: engine.Status) => Promise<unknown>) {
    const t = menu!.t
    menu = null
    run(() => fn(t))
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape') menu = null
  }

  // Torrents being removed right now: shown as such, and not removable twice.
  let removingNow = $state(new Set<string>())

  async function removeTorrent(hash: string, files: boolean) {
    if (removingNow.has(hash)) return
    removingNow = new Set([...removingNow, hash])
    if (selected === hash) selected = null // close its files panel before it's gone
    await run(() => Remove(hash, files))
    removingNow = new Set([...removingNow].filter(h => h !== hash))
  }

  let startingNow = $state(new Set<string>())

  async function startTorrent(hash: string) {
    if (startingNow.has(hash)) return
    startingNow = new Set([...startingNow, hash])
    await run(() => StartTorrent(hash))
    startingNow = new Set([...startingNow].filter(h => h !== hash))
  }

  function confirmRemove() {
    const t = removing!
    removing = null
    removeTorrent(t.hash, deleteFiles)
  }

  function eta(t: engine.Status): string {
    if (t.state !== 'downloading' || t.downRate <= 0) return '–'
    return duration(Math.round((t.size - t.done) / t.downRate))
  }

  // Antivirus status takes over the Status column once a torrent finishes.
  function statusText(t: engine.Status): string {
    if (t.verifying) return `Verifying ${Math.floor(t.verifyProgress * 100)}%`
    if (t.sfv?.checking) return 'Checking .sfv…'
    if (t.state === 'seeding' && t.scanState === 'scanning') return 'Scanning for malware…'
    if (t.scanState === 'threat') return '⚠ Threat found'
    if (removingNow.has(t.hash)) return 'Removing…'
    if (startingNow.has(t.hash)) return 'Starting…'
    if (t.confirm && t.state === 'paused') return 'Waiting for Start'
    return stateLabel[t.state] ?? t.state
  }

  const stateLabel: Record<string, string> = {
    metadata: 'Fetching metadata', 'needs-torrent-file': '⚠ Needs .torrent file', blocked: '⚠ Stopped', downloading: 'Downloading', checking: 'Checking', seeding: 'Seeding', paused: 'Paused',
  }

  const limitText = $derived.by(() => {
    if (!settings) return ''
    const parts = []
    if (settings.downLimitKB > 0) parts.push(`↓ ≤ ${settings.downLimitKB} KB/s`)
    if (settings.upLimitKB > 0) parts.push(`↑ ≤ ${settings.upLimitKB} KB/s`)
    return parts.join('  ')
  })
</script>

<div class="shell" style="--wails-drop-target: drop">
  <header>
    <div class="brand">
      <svg viewBox="0 0 32 32" width="22" height="22" aria-hidden="true">
        <rect x="1" y="1" width="30" height="30" rx="7" fill="#2370c4" />
        <path d="M6 25.6 Q10.9 8.3 22.4 5.4 Q17.9 16.6 25 25.6 Z" fill="#f2f6fa" />
        <path d="M1 22.5 Q8 19.5 13 22 T25 22 T31 21.5 V31 H1 Z" fill="#0b2e5e" />
        <path d="M1 22.5 Q8 19.5 13 22 T25 22 T31 21.5" fill="none" stroke="#a9d8ff" stroke-width="1.6" />
      </svg>
      <span>Shark Bite<b>Torrent</b></span>
    </div>
    <div class="vpn {vpn.state}">
      <span class="dot"></span>
      {#if vpn.state === 'connected'}
        <span class="text" title={vpn.server ? `Server: ${vpn.server}` : ''}>
          <b>VPN on</b> · {vpn.name} · exit {vpn.exitIP} · {vpn.location}{vpn.colo ? ` (${vpn.colo})` : ''}
          {#if vpn.port}· port {vpn.port} open{/if}
        </span>
        {#if vpn.error}<span class="warn" title={vpn.error}>⚠ check failed</span>{/if}
        {#if vpn.warning?.startsWith("couldn't switch")}
          <span class="warn" title={vpn.warning}>⚠ VPN switch failed</span>
          <button onclick={() => run(Reconnect)}>Retry</button>
        {:else if vpn.warning}
          <span class="warn" title={vpn.warning}>⚠ no port forwarding</span>
        {/if}
      {:else if vpn.state === 'off'}
        <span class="text" title="Chosen in Settings → VPN. Peers and trackers see your real IP address.">
          <b>VPN off</b> · torrents use your normal connection
        </span>
      {:else if vpn.state === 'connecting'}
        <span class="text"><b>Connecting {vpn.name || 'VPN'}…</b> Torrents start once the tunnel is verified.</span>
      {:else}
        <span class="text" title={vpn.error}><b>VPN error</b> · {vpn.error}</span>
        <button onclick={() => run(Reconnect)}>Retry</button>
      {/if}
    </div>
    <div class="totals" title={limitText ? `Speed limits: ${limitText}` : 'No speed limits'}>
      ↓ {rate(totalDown)} &nbsp; ↑ {rate(totalUp)}
      {#if limitText}<span class="limit">limited</span>{/if}
    </div>
    <button class="icon" onclick={() => (showSettings = true)} title="Settings" aria-label="Settings">⚙</button>
  </header>

  <div class="toolbar">
    <form onsubmit={addMagnet}>
      <input placeholder="Paste a magnet link…" bind:value={magnet} disabled={!connected} />
      <button class="primary" type="submit" disabled={!connected || !magnet.trim() || adding > 0}>{adding > 0 ? 'Adding…' : 'Add'}</button>
    </form>
    <button onclick={() => runAdding(PickTorrentFile)} disabled={!connected || adding > 0}>Open .torrent…</button>
    <button class:active={searching} onclick={() => (searching = !searching)} disabled={!connected}>
      {searching ? '← Torrents' : 'Search'}
    </button>
    <span class="sep"></span>
    <button onclick={() => sel && run(() => Resume(sel.hash, (sel.warnings ?? []).length))} disabled={!sel || sel.state !== 'paused'}>Resume</button>
    <button onclick={() => sel && run(() => Pause(sel.hash))} disabled={!sel || sel.state === 'paused'}>Pause</button>
    <button onclick={() => { removing = sel ?? null; deleteFiles = false }} disabled={!sel || removingNow.has(sel.hash)}>Remove…</button>
  </div>

  {#each torrents.filter(t => t.confirm) as t (t.hash)}
    <div class="update" role="status">
      {#if t.state === 'blocked'}
        <span>
          🧲 Added from outside the app: <b>{t.name}</b>. It can't start: {t.problem}
        </span>
      {:else}
        <span>
          🧲 Added from outside the app (a link or file you opened):
          <b>{t.name || 'unnamed torrent'}</b>{t.size ? ` (${bytes(t.size)})` : ''}. Nothing is downloaded or contacted until you start it.
          {#if (t.warnings ?? []).length > 0}<b class="warn">⚠ Some of its files look risky: {(t.warnings ?? []).map(w => w.reason).join(' ')}</b>{/if}
        </span>
        <button class="primary" onclick={() => startTorrent(t.hash)} disabled={startingNow.has(t.hash)}>
          {startingNow.has(t.hash) ? 'Starting…' : 'Start'}
        </button>
      {/if}
      <button onclick={() => removeTorrent(t.hash, false)} disabled={removingNow.has(t.hash)}>Remove</button>
    </div>
  {/each}

  {#if update}
    <div class="update" role="status">
      <span>🦈 <b>Shark BiteTorrent {update.latest}</b> is available (you have {update.current}).</span>
      {#if installing}
        <span class="muted">Downloading and verifying the update… The app restarts by itself when it's installed.</span>
      {:else}
        {#if askDirect}
          <span>The VPN isn't connected. Download over your normal connection? GitHub will see your real IP.</span>
          <button onclick={() => (askDirect === 'download' ? downloadUpdate(true) : installUpdate(true))}>Use normal connection</button>
          <button onclick={() => (askDirect = null)}>Cancel</button>
        {:else if downloaded}
          <InstallCommand path={downloaded.path} hint={downloaded.hint} command={downloaded.command} />
        {:else if update.canInstall}
          <button class="primary" onclick={() => installUpdate()}>Update now</button>
        {:else if update.canDownload}
          <button class="primary" onclick={() => downloadUpdate()}>Download (verified)</button>
        {:else}
          <button class="primary" onclick={() => OpenURL(update!.notesURL)}>Download</button>
        {/if}
        {#if (update.notes ?? []).length > 0}
          <button onclick={() => (showNotes = !showNotes)}>{showNotes ? 'Hide' : "What's new"}</button>
        {:else}
          <button onclick={() => OpenURL(update!.notesURL)}>What's new ↗</button>
        {/if}
        <button onclick={() => { SkipUpdate(update!.latest); update = null }}>Skip this version</button>
        <button class="close" onclick={() => (update = null)} aria-label="Later">✕</button>
      {/if}
      {#if showNotes && (update.notes ?? []).length > 0}
        <div class="update-notes">
          <ReleaseNotes notes={update.notes} />
          <button class="link" onclick={() => OpenURL(update!.notesURL)}>Release page ↗</button>
        </div>
      {/if}
    </div>
  {/if}

  {#if notice}<div class="notice" role="status"><span>{notice}</span> <button onclick={() => (notice = '')} aria-label="Dismiss">✕</button></div>{/if}
  {#if error}<div class="error" role="alert"><span>{error}</span> <button onclick={() => (error = '')} aria-label="Dismiss">✕</button></div>{/if}

  {#if searching}
    <main>
      <SearchView onAdded={refresh} onError={e => (error = e)} onOpenSettings={() => (showSettings = true)} />
    </main>
  {:else}
  <main class:split={!!sel}>
    <div class="list">
      {#if torrents.length === 0}
        <div class="empty">
          {#if connected}
            No torrents yet. Paste a magnet link, open a .torrent file, or drop one on this window.
          {:else}
            Waiting for the VPN. Nothing will download until the tunnel is up.
          {/if}
        </div>
      {:else}
        <table>
          <thead>
            <tr>
              <th class="name">Name</th><th>Size</th><th class="prog">Progress</th><th>Status</th>
              <th>Down</th><th>Up</th><th>ETA</th><th>Seeds/Peers</th>
            </tr>
          </thead>
          <tbody>
            {#each torrents as t (t.hash)}
              <tr
                class:selected={t.hash === selected}
                onclick={() => (selected = t.hash)}
                oncontextmenu={e => openMenu(e, t)}
              >
                <td class="name" title={t.name}>
                  {#if t.scanState === 'threat'}
                    <span class="flag threat" title="Antivirus found: {(t.threats ?? []).join(', ')}">⚠</span>
                  {:else if t.scanState === 'toolarge' && !t.scanSeen}
                    <span class="flag caution" title="Some files couldn't be fully scanned for malware. Click for details.">⚠</span>
                  {:else if (t.warnings ?? []).length > 0}
                    <span class="flag caution" title={(t.warnings ?? []).map(w => w.reason).join('\n')}>⚠</span>
                  {/if}
                  {t.name || t.hash}
                </td>
                <td>{t.size ? bytes(t.size) : '–'}</td>
                <td class="prog">
                  <div class="bar {t.state}"><div style="width:{(t.progress * 100).toFixed(1)}%"></div></div>
                  <span>{(t.progress * 100).toFixed(1)}%</span>
                </td>
                <td
                  class:warn={t.state === 'needs-torrent-file' || t.state === 'blocked'}
                  class:threat={t.scanState === 'threat'}
                  title={t.state === 'needs-torrent-file'
                    ? "Peers are connected but won't share the file list. This usually means a private torrent, which can't start from a magnet link. Download its .torrent file and open that instead."
                    : t.state === 'blocked' ? t.problem : ''}
                >{statusText(t)}</td>
                <td>{rate(t.downRate)}</td>
                <td>{rate(t.upRate)}</td>
                <td>{eta(t)}</td>
                <td>{t.seeds}/{t.peers}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    </div>
    {#if sel}
      <div class="details">
        <FilesPanel torrent={sel} onError={e => (error = e)} />
      </div>
    {/if}
  </main>
  {/if}

  <footer>
    <span class="path" title={settings?.downloadDir}>Downloads go to {settings?.downloadDir}</span>
    <button onclick={() => run(async () => { await ChooseDownloadDir(); await loadSettings() })}>Change download folder…</button>
  </footer>
</div>

{#if menu}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="menu-catcher" onclick={() => (menu = null)} oncontextmenu={e => { e.preventDefault(); menu = null }}></div>
  <div class="menu" role="menu" style="left:{menu.x}px; top:{menu.y}px">
    <button role="menuitem" onclick={() => menuAction(t => OpenFolder(t.hash))}>Show files</button>
    {#if menu.t.state === 'paused'}
      <button role="menuitem" onclick={() => menuAction(t => Resume(t.hash, (t.warnings ?? []).length))}>Resume</button>
    {:else}
      <button role="menuitem" onclick={() => menuAction(t => Pause(t.hash))}>Pause</button>
    {/if}
    <button
      role="menuitem"
      disabled={menu.t.verifying || menu.t.state === 'metadata' || menu.t.state === 'needs-torrent-file'}
      onclick={() => menuAction(t => VerifyTorrent(t.hash))}
    >Verify files</button>
    <div class="menu-sep"></div>
    <button role="menuitem" onclick={() => { removing = menu!.t; deleteFiles = false; menu = null }}>Remove…</button>
  </div>
{/if}

<svelte:window onkeydown={onKey} />

{#if dragging}
  <div class="drop-overlay" aria-hidden="true">
    <div>🦈 Drop .torrent files or magnet links to add them</div>
  </div>
{/if}

{#if showSettings}
  <Settings onClose={() => { showSettings = false; loadSettings(); refresh() }} />
{/if}

{#if removing}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={() => (removing = null)}>
    <div class="confirm" role="dialog" aria-modal="true" tabindex="-1" onclick={e => e.stopPropagation()}>
      <h3>Remove torrent?</h3>
      <p class="tname">{removing.name || removing.hash}</p>
      <label class="check">
        <input type="checkbox" bind:checked={deleteFiles} />
        <span>Also delete downloaded files</span>
      </label>
      {#if deleteFiles}<p class="danger">Files are deleted permanently, not moved to the Recycle Bin.</p>{/if}
      <div class="actions">
        <button onclick={() => (removing = null)}>Cancel</button>
        <button class:danger-btn={deleteFiles} class:primary={!deleteFiles} onclick={confirmRemove}>
          {deleteFiles ? 'Remove and delete' : 'Remove'}
        </button>
      </div>
    </div>
  </div>
{/if}

<style>
  .shell { display: flex; flex-direction: column; height: 100vh; }

  header {
    display: flex; align-items: center; gap: 16px;
    padding: 8px 14px; border-bottom: 1px solid var(--border); background: var(--panel);
  }
  .brand { display: flex; align-items: center; gap: 8px; font-size: 15px; white-space: nowrap; }
  .brand b { color: #6cb8ff; font-weight: 600; }
  .vpn { display: flex; align-items: center; gap: 10px; min-width: 0; flex: 1; }
  .vpn .text { white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .vpn button { padding: 3px 10px; }
  .dot { width: 10px; height: 10px; border-radius: 50%; flex: none; background: var(--warn); }
  .vpn.connected .dot { background: var(--ok); box-shadow: 0 0 6px var(--ok); }
  .vpn.error .dot { background: var(--err); }
  .vpn.off .dot { background: var(--warn); }
  .vpn.off b { color: var(--warn); }
  .vpn.connecting .dot { animation: pulse 1s infinite alternate; }
  @keyframes pulse { to { opacity: 0.3; } }
  .totals { color: var(--muted); white-space: nowrap; font-variant-numeric: tabular-nums; }
  .limit {
    margin-left: 6px; padding: 1px 6px; border-radius: 4px; font-size: 11px;
    background: rgba(210, 153, 34, 0.15); color: var(--warn);
  }
  .icon { padding: 2px 9px; font-size: 16px; }
  .toolbar button.active { border-color: var(--accent); color: var(--accent); }
  .warn { color: var(--warn); }
  td.threat { color: var(--err); font-weight: 600; }
  .flag { margin-right: 4px; font-weight: 700; }
  .flag.caution { color: var(--warn); }
  .flag.threat { color: var(--err); }

  .toolbar { display: flex; align-items: center; gap: 8px; padding: 10px 14px; }
  .toolbar form { display: flex; gap: 8px; flex: 1; min-width: 0; }
  .toolbar input { flex: 1; min-width: 0; }
  .sep { width: 1px; align-self: stretch; background: var(--border); margin: 0 4px; }

  .update {
    margin: 0 14px 8px; padding: 8px 12px; border-radius: 6px;
    background: rgba(79, 140, 255, 0.12); border: 1px solid rgba(79, 140, 255, 0.4);
    display: flex; align-items: center; gap: 8px; flex-wrap: wrap;
  }
  .update > span:first-child { flex: 1; min-width: 240px; }
  .update button { padding: 3px 10px; }
  .update .close { background: none; border: none; }
  .update-notes { flex-basis: 100%; border-top: 1px solid rgba(79, 140, 255, 0.3); padding-top: 8px; }
  .update-notes .link { background: none; border: none; padding: 4px 0 0; color: var(--accent); text-decoration: underline; }
  .muted { color: var(--muted); }

  .error {
    margin: 0 14px 8px; padding: 8px 12px; border-radius: 6px;
    background: rgba(248, 81, 73, 0.12); border: 1px solid rgba(248, 81, 73, 0.4);
    display: flex; justify-content: space-between; align-items: center; gap: 8px; user-select: text;
  }
  .error button { padding: 2px 8px; background: none; border: none; }
  .notice {
    margin: 0 14px 8px; padding: 8px 12px; border-radius: 6px;
    background: rgba(210, 153, 34, 0.12); border: 1px solid rgba(210, 153, 34, 0.45);
    display: flex; justify-content: space-between; align-items: center; gap: 8px; user-select: text;
  }
  .notice button { padding: 2px 8px; background: none; border: none; }

  main { flex: 1; min-height: 0; display: flex; flex-direction: column; border-top: 1px solid var(--border); }
  .list { flex: 1; min-height: 0; overflow: auto; }
  main.split .list { flex: 1.4; }
  .details { flex: 1; min-height: 120px; border-top: 1px solid var(--border); }
  .empty { display: grid; place-items: center; height: 100%; color: var(--muted); padding: 0 16px; text-align: center; }

  table { width: 100%; border-collapse: collapse; font-variant-numeric: tabular-nums; }
  th {
    position: sticky; top: 0; background: var(--bg); color: var(--muted); font-weight: 500;
    text-align: left; padding: 8px 10px; border-bottom: 1px solid var(--border); white-space: nowrap;
  }
  td { padding: 7px 10px; border-bottom: 1px solid var(--border); white-space: nowrap; }
  td.name, th.name { width: 40%; max-width: 0; overflow: hidden; text-overflow: ellipsis; }
  tbody tr { cursor: default; }
  tbody tr:hover { background: var(--row-hover); }
  tbody tr.selected { background: rgba(79, 140, 255, 0.18); }

  .prog { width: 160px; }
  td.prog { display: flex; align-items: center; gap: 8px; }
  .bar { flex: 1; height: 6px; border-radius: 3px; background: var(--border); overflow: hidden; }
  .bar div { height: 100%; background: var(--accent); transition: width 0.4s; }
  .bar.seeding div { background: var(--ok); }
  .bar.paused div { background: var(--muted); }
  td.prog span { width: 44px; text-align: right; color: var(--muted); font-size: 12px; }

  footer {
    display: flex; align-items: center; gap: 8px; padding: 8px 14px;
    border-top: 1px solid var(--border); background: var(--panel); color: var(--muted);
  }
  footer .path { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  footer button { padding: 4px 10px; }

  .drop-overlay {
    position: fixed; inset: 0; z-index: 30; pointer-events: none;
    display: grid; place-items: center;
    background: rgba(24, 26, 31, 0.75); border: 3px dashed var(--accent); border-radius: 8px;
  }
  .drop-overlay div { font-size: 18px; padding: 16px 24px; background: var(--panel); border-radius: 10px; }

  .menu-catcher { position: fixed; inset: 0; z-index: 20; }
  .menu {
    position: fixed; z-index: 21; min-width: 170px; padding: 4px;
    background: var(--panel); border: 1px solid var(--border); border-radius: 8px;
    box-shadow: 0 10px 30px rgba(0, 0, 0, 0.5);
    display: flex; flex-direction: column;
  }
  .menu button { border: none; background: none; text-align: left; padding: 6px 10px; border-radius: 5px; }
  .menu button:hover:not(:disabled) { background: var(--row-hover); }
  .menu button:disabled { opacity: 0.45; cursor: default; }
  .menu-sep { height: 1px; background: var(--border); margin: 4px 2px; }

  .backdrop { position: fixed; inset: 0; background: rgba(0, 0, 0, 0.55); display: grid; place-items: center; z-index: 10; }
  .confirm {
    width: min(440px, calc(100vw - 32px)); padding: 18px 20px;
    background: var(--panel); border: 1px solid var(--border); border-radius: 10px;
    box-shadow: 0 20px 50px rgba(0, 0, 0, 0.5);
  }
  .confirm h3 { margin: 0 0 6px; font-size: 16px; font-weight: 600; }
  .tname { margin: 0 0 14px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .check { display: flex; align-items: center; gap: 8px; cursor: pointer; }
  .danger { color: var(--err); font-size: 13px; margin: 8px 0 0; }
  .actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 18px; }
  .danger-btn { background: var(--err); border-color: var(--err); color: #fff; }
</style>
