<script lang="ts">
  import { ReadPaste, AddLinksTo } from '../wailsjs/go/main/App.js'
  import type { main, paste } from '../wailsjs/go/models'

  let { onClose, onDone, askChoice }: {
    onClose: () => void
    onDone: (summary: string) => void
    askChoice: (what: string) => Promise<main.AddChoice | null> // where to save them (App.svelte)
  } = $props()

  let input = $state('')
  let links = $state<paste.Link[] | null>(null)
  let picked = $state(new Set<string>())
  let busy = $state(false)
  let error = $state('')

  const kindLabel: Record<string, string> = {
    magnet: 'Torrent', torrent: 'Torrent', file: 'File', page: 'File host',
  }

  async function find(ev: SubmitEvent) {
    ev.preventDefault()
    if (!input.trim()) return
    busy = true
    error = ''
    try {
      const found = await ReadPaste(input)
      links = found
      // Torrents and files are what the user is after; pages only when the
      // paste is nothing but pages (a list of file-host links), since a
      // paste of files often links to unrelated pages too.
      const direct = found.filter(l => l.kind !== 'page')
      picked = new Set((direct.length ? direct : found).map(l => l.url))
    } catch (e) {
      error = String(e)
    }
    busy = false
  }

  function toggle(url: string) {
    const s = new Set(picked)
    if (s.has(url)) s.delete(url)
    else s.add(url)
    picked = s
  }

  async function download() {
    const chosen = links!.filter(l => picked.has(l.url))
    const c = await askChoice(chosen.length === 1 ? show(chosen[0]) : `${chosen.length} links`)
    if (!c) return
    busy = true
    error = ''
    try {
      const res = await AddLinksTo(chosen, c)
      const parts = []
      if (res.files) parts.push(`${res.files} file${res.files > 1 ? 's' : ''} downloading`)
      if (res.torrents) parts.push(`${res.torrents} torrent${res.torrents > 1 ? 's' : ''} added`)
      if (res.pages) parts.push(`${res.pages} file-host page${res.pages > 1 ? 's' : ''} opening in a browser window, which gets each download by itself`)
      if (res.errors.length) {
        error = res.errors.join('\n')
        if (parts.length) onDone(parts.join(', ') + '.')
      } else {
        onDone(parts.join(', ') + '.')
        onClose()
      }
    } catch (e) {
      error = String(e)
    }
    busy = false
  }

  const show = (l: paste.Link) => l.name || l.url
</script>

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
<div class="backdrop" onclick={onClose}>
  <div class="dialog" role="dialog" aria-modal="true" aria-labelledby="paste-title" tabindex="-1" onclick={e => e.stopPropagation()}>
    <h3 id="paste-title">Paste links</h3>
    <p class="muted">
      Links, a page that lists downloads, or a paste link (encrypted pastes are opened on this computer).
      Everything goes through the VPN.
    </p>
    <form onsubmit={find}>
      <textarea bind:value={input} rows="4" placeholder="Paste here…" disabled={busy}></textarea>
      <div class="row">
        <button class="primary" type="submit" disabled={busy || !input.trim()}>{busy && !links ? 'Reading…' : 'Find links'}</button>
      </div>
    </form>

    {#if links}
      <div class="head">
        <span>{links.length} link{links.length > 1 ? 's' : ''} found</span>
        <button class="link" onclick={() => (picked = new Set(links!.map(l => l.url)))}>All</button>
        <button class="link" onclick={() => (picked = new Set())}>None</button>
      </div>
      <ul>
        {#each links as l (l.url)}
          <li>
            <label title={l.url}>
              <input type="checkbox" checked={picked.has(l.url)} onchange={() => toggle(l.url)} />
              <span class="kind {l.kind}">{kindLabel[l.kind] ?? l.kind}</span>
              <span class="name">{show(l)}</span>
              {#if l.host}<span class="host">{l.host}</span>{/if}
            </label>
          </li>
        {/each}
      </ul>
      {#if links.some(l => l.kind === 'page' && picked.has(l.url))}
        <p class="muted small">
          File-host pages open in a browser window of the app's own, through the VPN. It clicks each page's download
          button and blocks pop-ups. If a page asks you to prove you're human, do that in the window and it carries on.
        </p>
      {/if}
    {/if}

    {#if error}<p class="err">{error}</p>{/if}

    <div class="actions">
      <button onclick={onClose}>Cancel</button>
      {#if links}
        <button class="primary" onclick={download} disabled={busy || picked.size === 0}>
          {busy ? 'Adding…' : `Download ${picked.size}`}
        </button>
      {/if}
    </div>
  </div>
</div>

<style>
  .backdrop { position: fixed; inset: 0; background: rgba(0, 0, 0, 0.55); display: grid; place-items: center; z-index: 10; }
  .dialog {
    width: min(680px, calc(100vw - 32px)); max-height: calc(100vh - 48px); overflow: auto; padding: 18px 20px;
    background: var(--panel); border: 1px solid var(--border); border-radius: 10px;
    box-shadow: 0 20px 50px rgba(0, 0, 0, 0.5);
    display: flex; flex-direction: column;
  }
  h3 { margin: 0 0 6px; font-size: 16px; font-weight: 600; }
  .muted { color: var(--muted); margin: 0 0 10px; }
  .small { font-size: 12px; margin-top: 8px; }
  textarea { width: 100%; box-sizing: border-box; resize: vertical; font: inherit; }
  .row { display: flex; justify-content: flex-end; margin-top: 8px; }
  .head { display: flex; align-items: center; gap: 10px; margin: 14px 0 6px; color: var(--muted); }
  .head span { flex: 1; }
  .link { background: none; border: none; padding: 0; color: var(--accent); }
  ul { list-style: none; margin: 0; padding: 0; max-height: 300px; overflow: auto; border: 1px solid var(--border); border-radius: 6px; }
  li { border-bottom: 1px solid var(--border); }
  li:last-child { border-bottom: none; }
  label { display: flex; align-items: center; gap: 8px; padding: 6px 10px; cursor: pointer; min-width: 0; }
  label:hover { background: var(--row-hover); }
  .kind { flex: none; font-size: 11px; padding: 1px 6px; border-radius: 4px; background: var(--border); color: var(--muted); }
  .kind.file { background: rgba(79, 140, 255, 0.18); color: var(--accent); }
  .kind.magnet, .kind.torrent { background: rgba(63, 185, 80, 0.15); color: var(--ok); }
  .name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .host { flex: none; color: var(--muted); font-size: 12px; }
  .err { color: var(--err); white-space: pre-wrap; user-select: text; margin: 10px 0 0; }
  .actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 16px; }
</style>
