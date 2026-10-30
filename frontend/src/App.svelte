<script lang="ts">
  import InstallCommand from './InstallCommand.svelte'
  import { onMount, tick } from 'svelte'
  import {
    GetVpnStatus, ListTorrents, GetSettings,
    Pause, Resume, Remove, Reconnect, OpenFolder, ChooseDownloadDir,
    InstallUpdate, DownloadUpdate, SkipUpdate, OpenURL, VerifyTorrent, StartTorrent,
    ListDownloads, PauseDownload, ResumeDownload, RemoveDownload, GetNewLink, OpenDownloadFolder,
    AddMagnetTo, ChooseTorrentFiles, AddTorrentFiles, StartTorrentIn, ChooseDirectory,
  } from '../wailsjs/go/main/App.js'
  import { EventsOn, OnFileDrop } from '../wailsjs/runtime/runtime.js'
  import type { engine, main } from '../wailsjs/go/models'
  import FilesPanel from './FilesPanel.svelte'
  import PasteLinks from './PasteLinks.svelte'
  import AddDialog from './AddDialog.svelte'
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
  // Direct downloads ("Paste links"), listed above the torrents.
  let downloads = $state<main.DownloadStatus[]>([])
  // Several can be selected (Ctrl/Shift+click): pastes bring in many parts.
  let selectedDls = $state(new Set<string>())
  let lastDl: string | null = null // where a Shift+click range starts
  let showPaste = $state(false)
  let removingDls = $state<main.DownloadStatus[] | null>(null)

  // "off" is the user's explicit No VPN choice; the engine runs either way.
  const connected = $derived(vpn.state === 'connected' || vpn.state === 'off')
  const sel = $derived(torrents.find(t => t.hash === selected))
  const selDls = $derived(downloads.filter(d => selectedDls.has(d.id)))
  const finishedDls = $derived(downloads.filter(d => d.state === 'done'))
  const totalDown = $derived(torrents.reduce((s, t) => s + t.downRate, 0) + downloads.reduce((s, d) => s + d.rate, 0))
  const totalUp = $derived(torrents.reduce((s, t) => s + t.upRate, 0))

  async function refresh() {
    try {
      vpn = await GetVpnStatus()
      torrents = await ListTorrents()
      if (selected && !torrents.some(t => t.hash === selected)) selected = null
      downloads = await ListDownloads()
      if ([...selectedDls].some(id => !downloads.some(d => d.id === id))) {
        selectedDls = new Set([...selectedDls].filter(id => downloads.some(d => d.id === id)))
      }
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
    const offChannel = EventsOn('update-channel-changed', () => (update = null))
    const offNotice = EventsOn('notice', (msg: string) => (notice = msg))
    // .torrent files dropped on the window, when the user wants to be asked first.
    const offAskAdd = EventsOn('ask-add', (paths: string[]) => addTorrentFiles(paths))
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
      offChannel()
      offNotice()
      offAskAdd()
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

  // Where to save what's being added. With "Ask where to save each
  // download" on, the dialog asks (null: cancelled); otherwise the default.
  let asking = $state<{ what: string; resolve: (c: main.AddChoice | null) => void } | null>(null)
  function askChoice(what: string): Promise<main.AddChoice | null> {
    if (!settings?.askSaveLocation) return Promise.resolve({ dir: '', paused: false } as main.AddChoice)
    return new Promise(resolve => (asking = { what, resolve }))
  }
  function answer(c: main.AddChoice | null) {
    const a = asking!
    asking = null
    a.resolve(c)
    loadSettings() // recent folders, or "don't ask again"
  }

  const magnetName = (link: string) =>
    new URLSearchParams(link.slice(link.indexOf('?') + 1)).get('dn') || 'Magnet link'
  const plural = (n: number, one: string) => `${n} ${one}${n > 1 ? 's' : ''}`

  async function addMagnet(ev: SubmitEvent) {
    ev.preventDefault()
    const link = magnet.trim()
    if (!link) return
    const c = await askChoice(magnetName(link))
    if (!c) return
    runAdding(async () => {
      await AddMagnetTo(link, c)
      magnet = ''
    })
  }

  async function addTorrentFiles(paths: string[]) {
    if (paths.length === 0) return
    const name = (p: string) => p.split(/[\\/]/).pop()!
    const c = await askChoice(paths.length === 1 ? name(paths[0]) : plural(paths.length, 'torrent file'))
    if (c) runAdding(() => AddTorrentFiles(paths, c))
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
    askChoice(links.length === 1 ? magnetName(links[0]) : plural(links.length, 'magnet link')).then(c => {
      if (c) run(async () => {
        for (const l of links) await AddMagnetTo(l, c)
      })
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
    selectedDls = new Set()
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
    if (e.key === 'Escape') {
      menu = null
      dlMenu = null
    }
    // Keys for the downloads list, unless typing somewhere or a dialog is open.
    const typing = (e.target as HTMLElement)?.closest?.('input, textarea, select')
    if (typing || showPaste || showSettings || removing || removingDls || selDls.length === 0) return
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
      e.preventDefault()
      selectedDls = new Set(downloads.map(d => d.id))
    } else if (e.key === 'Delete') {
      askRemoveDownloads(selDls)
    }
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
    await run(() => StartTorrentIn(hash, bannerDir[hash] ?? ''))
    startingNow = new Set([...startingNow].filter(h => h !== hash))
  }

  // The folder picked in a torrent's "added from outside" banner ('' = default).
  let bannerDir = $state<Record<string, string>>({})
  let browsedDirs = $state<string[]>([])
  const bannerDirs = $derived([...new Set([...browsedDirs, ...(settings?.recentDirs ?? [])])])
  async function pickBannerDir(hash: string, el: HTMLSelectElement) {
    let value = el.value
    if (value === '?browse') {
      const d = await ChooseDirectory(bannerDir[hash] || settings?.downloadDir || '')
      if (d) browsedDirs = [d, ...browsedDirs.filter(x => x !== d)]
      value = d || bannerDir[hash] || ''
    }
    bannerDir = { ...bannerDir, [hash]: value }
    await tick()
    el.value = value // back from "Browse…" if that was cancelled
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

  function dlStatus(d: main.DownloadStatus): string {
    if (removingNow.has(d.id)) return 'Removing…'
    if (d.state === 'running') return d.waiting ? 'Waiting for its folder' : d.offline ? 'Waiting for the VPN' : 'Downloading'
    if (d.state === 'needsLink') return '⚠ Needs a new link'
    if (d.state === 'failed') return '⚠ Failed'
    if (d.state === 'done') {
      // As with torrents, the malware scan takes over once it's finished.
      if (d.scan === 'scanning') return 'Scanning for malware…'
      if (d.scan === 'threat') return '⚠ Threat found'
      if (d.scan === 'gone') return '⚠ File removed'
      if (d.scan === '') return 'Checking…'
    }
    return dlLabel[d.state] ?? d.state
  }
  const dlLabel: Record<string, string> = { queued: 'Queued', paused: 'Paused', done: 'Done' }

  // The status cell's tooltip: the error, or what the scan found.
  function dlStatusTitle(d: main.DownloadStatus): string {
    if (d.state !== 'done') return d.waiting || d.error
    switch (d.scan) {
      case 'clean': return `Scanned with ${d.scanEngine}: no known threats.`
      case 'threat': return `${d.scanEngine} found: ${d.threats.join(', ')}. Don't open it unless you're sure it's a false alarm.`
      case 'gone': return d.threats.length
        ? `${d.scanEngine} found ${d.threats.join(', ')} and removed the file. If you're sure it's a false alarm, restore it in your antivirus.`
        : 'The file is no longer there. Your antivirus may have quarantined it; check its history.'
      case 'off': return `Not scanned: ${d.scanError}.`
      case 'error': return `Couldn't scan it: ${d.scanError}`
    }
    return ''
  }

  function dlEta(d: main.DownloadStatus): string {
    if (d.state !== 'running' || d.rate <= 0 || d.size <= 0) return '–'
    return duration(Math.round((d.size - d.have) / d.rate))
  }

  const dlProgress = (d: main.DownloadStatus) => (d.size > 0 ? d.have / d.size : d.state === 'done' ? 1 : 0)

  // Click selects one download, Ctrl+click adds or removes one, Shift+click
  // selects a range, as in a file manager.
  function selectDownload(id: string, e?: MouseEvent) {
    selected = null
    if (e && (e.ctrlKey || e.metaKey)) {
      const s = new Set(selectedDls)
      if (s.has(id)) s.delete(id)
      else s.add(id)
      selectedDls = s
    } else if (e?.shiftKey && lastDl) {
      const ids = downloads.map(d => d.id)
      const [a, b] = [ids.indexOf(lastDl), ids.indexOf(id)].sort((x, y) => x - y)
      selectedDls = new Set(a < 0 ? [id] : ids.slice(a, b + 1))
      return // the range keeps its starting point
    } else {
      selectedDls = new Set([id])
    }
    lastDl = id
  }

  const canResume = (d: main.DownloadStatus) => d.state === 'paused' || d.state === 'failed'
  const canPause = (d: main.DownloadStatus) => d.state === 'running' || d.state === 'queued'

  // Pause or resume every selected download that can be.
  function eachDl(list: main.DownloadStatus[], ok: (d: main.DownloadStatus) => boolean, fn: (id: string) => Promise<unknown>) {
    run(async () => {
      for (const d of list.filter(ok)) await fn(d.id)
    })
  }

  async function removeDownloads(list: main.DownloadStatus[], files: boolean) {
    const ids = list.map(d => d.id).filter(id => !removingNow.has(id))
    removingNow = new Set([...removingNow, ...ids])
    selectedDls = new Set([...selectedDls].filter(id => !ids.includes(id)))
    // All at once: one file the antivirus is still holding mustn't hold up the rest.
    await run(async () => {
      const failed = (await Promise.allSettled(ids.map(id => RemoveDownload(id, files)))).filter(r => r.status === 'rejected')
      if (failed.length) throw (failed[0] as PromiseRejectedResult).reason
    })
    removingNow = new Set([...removingNow].filter(h => !ids.includes(h)))
  }

  function askRemoveDownloads(list: main.DownloadStatus[]) {
    if (list.length === 0) return
    removingDls = list
    deleteFiles = false
  }

  // Right-click menu on a download row. Its Pause/Resume/Remove act on the
  // whole selection when the clicked row is part of it.
  let dlMenu = $state<{ x: number; y: number; d: main.DownloadStatus } | null>(null)
  const menuDls = $derived(dlMenu ? (selectedDls.has(dlMenu.d.id) ? selDls : [dlMenu.d]) : [])

  function openDlMenu(e: MouseEvent, d: main.DownloadStatus) {
    e.preventDefault()
    if (!selectedDls.has(d.id)) selectDownload(d.id)
    const w = 190, h = 140
    dlMenu = { x: Math.min(e.clientX, window.innerWidth - w - 4), y: Math.min(e.clientY, window.innerHeight - h - 4), d }
  }

  function dlMenuAction(fn: (d: main.DownloadStatus) => Promise<unknown>) {
    const d = dlMenu!.d
    dlMenu = null
    run(() => fn(d))
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
    <button onclick={async () => addTorrentFiles(await ChooseTorrentFiles())} disabled={!connected || adding > 0}>Open .torrent…</button>
    <button onclick={() => (showPaste = true)} disabled={!connected} title="Download every link in some text, a page or a paste, through the VPN">Paste links…</button>
    <button class:active={searching} onclick={() => (searching = !searching)} disabled={!connected}>
      {searching ? '← Downloads' : 'Search'}
    </button>
    <span class="sep"></span>
    {#if selDls.length > 0}
      <button onclick={() => eachDl(selDls, canResume, ResumeDownload)} disabled={!selDls.some(canResume)}>Resume</button>
      <button onclick={() => eachDl(selDls, canPause, PauseDownload)} disabled={!selDls.some(canPause)}>Pause</button>
      <button onclick={() => askRemoveDownloads(selDls)} disabled={selDls.every(d => removingNow.has(d.id))}>
        {selDls.length > 1 ? `Remove ${selDls.length}…` : 'Remove…'}
      </button>
    {:else}
      <button onclick={() => sel && run(() => Resume(sel.hash, (sel.warnings ?? []).length))} disabled={!sel || sel.state !== 'paused'}>Resume</button>
      <button onclick={() => sel && run(() => Pause(sel.hash))} disabled={!sel || sel.state === 'paused'}>Pause</button>
      <button onclick={() => { removing = sel ?? null; deleteFiles = false }} disabled={!sel || removingNow.has(sel.hash)}>Remove…</button>
    {/if}
    {#if finishedDls.length > 0}
      <button title="Take finished downloads off the list. Their files stay where they are."
        onclick={() => removeDownloads(finishedDls, false)}>Clear finished ({finishedDls.length})</button>
    {/if}
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
        <label class="saveto">
          <span class="muted">Save to</span>
          <select value={bannerDir[t.hash] ?? ''} onchange={e => pickBannerDir(t.hash, e.target as HTMLSelectElement)}>
            <option value="">{settings?.downloadDir} (default)</option>
            {#each bannerDirs.filter(d => d !== settings?.downloadDir) as d (d)}<option value={d}>{d}</option>{/each}
            <option value="?browse">Browse…</option>
          </select>
        </label>
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
      <SearchView onAdded={refresh} onError={e => (error = e)} onOpenSettings={() => (showSettings = true)} {askChoice} />
    </main>
  {:else}
  <main class:split={!!sel}>
    <div class="list">
      {#if downloads.length > 0}
        <table class="downloads">
          <thead>
            <tr>
              <th class="name">Downloads</th><th>Size</th><th class="prog">Progress</th><th>Status</th>
              <th>Down</th><th>ETA</th>
            </tr>
          </thead>
          <tbody>
            {#each downloads as d (d.id)}
              <tr class:selected={selectedDls.has(d.id)} onclick={e => selectDownload(d.id, e)} oncontextmenu={e => openDlMenu(e, d)}>
                <td class="name" title={d.name}>
                  {#if d.scan === 'threat' || (d.scan === 'gone' && d.threats.length)}
                    <span class="flag threat" title="{d.scanEngine} found: {d.threats.join(', ')}">⚠</span>
                  {:else if (d.warnings ?? []).length > 0}
                    <span class="flag caution" title={(d.warnings ?? []).map(w => w.reason).join('\n')}>⚠</span>
                  {/if}
                  {d.name || 'Starting…'}
                </td>
                <td>{d.size > 0 ? bytes(d.size) : '–'}</td>
                <td class="prog">
                  <div class="bar" class:seeding={d.state === 'done'} class:paused={d.state === 'paused'}><div style="width:{(dlProgress(d) * 100).toFixed(1)}%"></div></div>
                  <span>{(dlProgress(d) * 100).toFixed(1)}%</span>
                </td>
                <td class:warn={d.state === 'needsLink' || d.state === 'failed' || d.scan === 'gone'} class:threat={d.scan === 'threat'} title={dlStatusTitle(d)}>
                  {dlStatus(d)}
                  {#if d.state === 'needsLink'}<button class="inline" onclick={e => { e.stopPropagation(); run(() => GetNewLink(d.id)) }}>Get a new link</button>{/if}
                </td>
                <td>{rate(d.rate)}</td>
                <td>{dlEta(d)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
      {#if torrents.length === 0 && downloads.length === 0}
        <div class="empty">
          {#if connected}
            Nothing here yet. Add a torrent: paste a magnet link, open a .torrent file, or drop one on this window. To download files from links, a web page or a paste, use Paste links.
          {:else}
            Waiting for the VPN. Nothing will download until the tunnel is up.
          {/if}
        </div>
      {:else if torrents.length > 0}
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
                onclick={() => { selected = t.hash; selectedDls = new Set() }}
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

{#if dlMenu}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="menu-catcher" onclick={() => (dlMenu = null)} oncontextmenu={e => { e.preventDefault(); dlMenu = null }}></div>
  <div class="menu" role="menu" style="left:{dlMenu.x}px; top:{dlMenu.y}px">
    <button role="menuitem" onclick={() => dlMenuAction(d => OpenDownloadFolder(d.id))}>{dlMenu.d.state === 'done' ? 'Show file' : 'Show folder'}</button>
    {#if menuDls.some(canResume)}
      <button role="menuitem" onclick={() => { const l = menuDls; dlMenu = null; eachDl(l, canResume, ResumeDownload) }}>
        {menuDls.every(d => d.state === 'failed') ? 'Try again' : 'Resume'}{menuDls.length > 1 ? ` (${menuDls.filter(canResume).length})` : ''}
      </button>
    {/if}
    {#if menuDls.some(canPause)}
      <button role="menuitem" onclick={() => { const l = menuDls; dlMenu = null; eachDl(l, canPause, PauseDownload) }}>
        Pause{menuDls.length > 1 ? ` (${menuDls.filter(canPause).length})` : ''}
      </button>
    {/if}
    {#if dlMenu.d.page && dlMenu.d.state !== 'done'}
      <button role="menuitem" onclick={() => dlMenuAction(d => GetNewLink(d.id))}>Get a new link</button>
    {/if}
    <div class="menu-sep"></div>
    <button role="menuitem" onclick={() => { const l = menuDls; dlMenu = null; askRemoveDownloads(l) }}>
      {menuDls.length > 1 ? `Remove ${menuDls.length}…` : 'Remove…'}
    </button>
  </div>
{/if}

{#if showPaste}
  <PasteLinks onClose={() => (showPaste = false)} onDone={msg => { notice = msg; refresh() }} {askChoice} />
{/if}

{#if asking}
  <AddDialog what={asking.what} defaultDir={settings?.downloadDir ?? ''} recentDirs={settings?.recentDirs ?? []}
    onConfirm={answer} onCancel={() => answer(null)} />
{/if}

{#if removingDls}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={() => (removingDls = null)}>
    <div class="confirm" role="dialog" aria-modal="true" tabindex="-1" onclick={e => e.stopPropagation()}>
      {#if removingDls.length === 1}
        <h3>Remove download?</h3>
        <p class="tname">{removingDls[0].name || 'Unnamed download'}</p>
      {:else}
        <h3>Remove {removingDls.length} downloads?</h3>
        <p class="tname" title={removingDls.map(d => d.name).join('\n')}>{removingDls.map(d => d.name || 'Unnamed').join(', ')}</p>
      {/if}
      <label class="check">
        <input type="checkbox" bind:checked={deleteFiles} />
        <span>
          {#if removingDls.every(d => d.state === 'done')}Also delete the {removingDls.length > 1 ? 'files' : 'file'}
          {:else}Also delete what was downloaded{/if}
        </span>
      </label>
      {#if deleteFiles}<p class="danger">Files are deleted permanently, not moved to the Recycle Bin.</p>{/if}
      <div class="actions">
        <button onclick={() => (removingDls = null)}>Cancel</button>
        <button class:danger-btn={deleteFiles} class:primary={!deleteFiles} onclick={() => { const l = removingDls!; removingDls = null; removeDownloads(l, deleteFiles) }}>
          {deleteFiles ? 'Remove and delete' : 'Remove'}
        </button>
      </div>
    </div>
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
  .saveto { display: flex; align-items: center; gap: 6px; }
  .saveto select { max-width: 260px; }
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
  table.downloads { margin-bottom: 6px; }
  .inline { margin-left: 8px; padding: 1px 8px; font-size: 12px; }

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
