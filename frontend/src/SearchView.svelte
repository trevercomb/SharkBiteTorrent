<script lang="ts">
  import { GetSearchStatus, AutoDetectSearch, SearchTorrents, AddSearchResultTo, OpenURL } from '../wailsjs/go/main/App.js'
  import type { main, search } from '../wailsjs/go/models'
  import { bytes } from './format'

  let { onAdded, onError, onOpenSettings, askChoice }: {
    onAdded: () => void
    onError: (e: string) => void
    onOpenSettings: () => void
    askChoice: (what: string) => Promise<main.AddChoice | null> // where to save it (App.svelte)
  } = $props()

  let status = $state<main.SearchStatus | null>(null)
  let query = $state('')
  let category = $state('')
  let results = $state<search.Result[]>([])
  let searched = $state('')
  let busy = $state(false)
  let detecting = $state(false)
  let adding = $state<Record<string, 'adding' | 'added'>>({})
  let sortKey = $state<'seeders' | 'leechers' | 'size' | 'published' | 'title' | 'indexer'>('seeders')
  let sortDesc = $state(true)
  let input = $state<HTMLInputElement | null>(null)

  GetSearchStatus().then(s => {
    status = s
    queueMicrotask(() => input?.focus())
  })

  async function autodetect() {
    detecting = true
    try {
      status = await AutoDetectSearch()
    } catch (e) {
      onError(String(e))
    }
    detecting = false
  }

  async function run(ev?: SubmitEvent) {
    ev?.preventDefault()
    if (!query.trim()) return
    busy = true
    onError('')
    try {
      results = (await SearchTorrents(query, category)) ?? []
      searched = query
      adding = {}
    } catch (e) {
      onError(String(e))
    }
    busy = false
  }

  const key = (r: search.Result) => r.indexer + '|' + r.title + '|' + r.size

  async function add(r: search.Result) {
    const k = key(r)
    if (adding[k]) return
    const c = await askChoice(r.title)
    if (!c) return
    adding = { ...adding, [k]: 'adding' }
    try {
      await AddSearchResultTo(r, c)
      adding = { ...adding, [k]: 'added' }
      onAdded()
    } catch (e) {
      const { [k]: _, ...rest } = adding
      adding = rest
      onError(String(e))
    }
  }

  function sortBy(k: typeof sortKey) {
    if (sortKey === k) sortDesc = !sortDesc
    else {
      sortKey = k
      sortDesc = k !== 'title' && k !== 'indexer'
    }
  }

  const sorted = $derived.by(() => {
    const dir = sortDesc ? -1 : 1
    return [...results].sort((a, b) => {
      let x: number | string, y: number | string
      switch (sortKey) {
        case 'size': x = a.size; y = b.size; break
        case 'published': x = Date.parse(a.published) || 0; y = Date.parse(b.published) || 0; break
        case 'title': x = a.title.toLowerCase(); y = b.title.toLowerCase(); break
        case 'indexer': x = a.indexer.toLowerCase(); y = b.indexer.toLowerCase(); break
        case 'leechers': x = a.leechers; y = b.leechers; break
        default: x = a.seeders; y = b.seeders
      }
      return x < y ? -dir : x > y ? dir : b.seeders - a.seeders
    })
  })

  function age(published: string): string {
    const t = Date.parse(published)
    if (!t) return '–'
    const h = (Date.now() - t) / 3.6e6
    if (h < 1) return 'now'
    if (h < 48) return `${Math.floor(h)}h`
    if (h < 24 * 60) return `${Math.floor(h / 24)}d`
    if (h < 24 * 730) return `${Math.floor(h / 24 / 30)}mo`
    return `${Math.floor(h / 24 / 365)}y`
  }

  const arrow = (k: typeof sortKey) => (sortKey === k ? (sortDesc ? ' ▾' : ' ▴') : '')
</script>

<div class="search">
  {#if status && !status.configured}
    <div class="setup">
      <h3>Search needs Prowlarr or Jackett</h3>
      <p class="muted">
        Shark BiteTorrent searches through your own Prowlarr or Jackett, which searches the sites you've added there.
        No sites are built in.
      </p>
      <div class="row">
        <button class="primary" onclick={autodetect} disabled={detecting}>{detecting ? 'Looking…' : 'Find it on this computer'}</button>
        <button onclick={onOpenSettings}>Enter address and API key…</button>
      </div>
      <p class="muted small">
        Don't have either? <button class="link" onclick={() => OpenURL('https://prowlarr.com')}>Prowlarr</button> and
        <button class="link" onclick={() => OpenURL('https://github.com/Jackett/Jackett')}>Jackett</button> are free.
      </p>
    </div>
  {:else if status}
    <form class="bar" onsubmit={run}>
      <input bind:this={input} bind:value={query} placeholder="Search your indexers…" aria-label="Search" />
      <select bind:value={category} aria-label="Category">
        <option value="">All</option>
        <option value="movies">Movies</option>
        <option value="tv">TV</option>
        <option value="music">Music</option>
        <option value="software">Software</option>
        <option value="books">Books</option>
      </select>
      <button class="primary" type="submit" disabled={busy || !query.trim()}>{busy ? 'Searching…' : 'Search'}</button>
    </form>
    <div class="meta muted">
      {#if busy}
        Searching every indexer in {status.kind === 'jackett' ? 'Jackett' : 'Prowlarr'}…
      {:else if searched}
        {results.length} results for “{searched}” · double-click a result to download
      {:else}
        Using {status.kind === 'jackett' ? 'Jackett' : 'Prowlarr'} at {status.url}
      {/if}
    </div>
    <div class="results">
      {#if sorted.length > 0}
        <table>
          <thead>
            <tr>
              <th class="name"><button class="th" onclick={() => sortBy('title')}>Name{arrow('title')}</button></th>
              <th class="num"><button class="th" onclick={() => sortBy('size')}>Size{arrow('size')}</button></th>
              <th class="num"><button class="th" onclick={() => sortBy('seeders')}>Seeds{arrow('seeders')}</button></th>
              <th class="num"><button class="th" onclick={() => sortBy('leechers')}>Peers{arrow('leechers')}</button></th>
              <th class="num"><button class="th" onclick={() => sortBy('published')}>Age{arrow('published')}</button></th>
              <th><button class="th" onclick={() => sortBy('indexer')}>Source{arrow('indexer')}</button></th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {#each sorted as r, i (i)}
              {@const st = adding[key(r)]}
              <tr ondblclick={() => add(r)} class:added={st === 'added'}>
                <td class="name" title={r.title}>{r.title}</td>
                <td class="num">{r.size ? bytes(r.size) : '–'}</td>
                <td class="num seeds" class:dead={r.seeders === 0}>{r.seeders}</td>
                <td class="num">{r.leechers}</td>
                <td class="num">{age(r.published)}</td>
                <td class="src">{r.indexer}</td>
                <td class="act">
                  {#if st === 'added'}
                    <span class="ok">Added ✓</span>
                  {:else}
                    <button onclick={() => add(r)} disabled={st === 'adding'}>{st === 'adding' ? 'Adding…' : 'Download'}</button>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      {:else if searched && !busy}
        <div class="empty muted">No results. Try different words, or check your indexers in {status.kind === 'jackett' ? 'Jackett' : 'Prowlarr'}.</div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .search { display: flex; flex-direction: column; height: 100%; min-height: 0; }
  .setup { max-width: 560px; margin: 48px auto; padding: 0 16px; display: grid; gap: 10px; }
  .setup h3 { margin: 0; font-size: 17px; font-weight: 600; }
  .setup p { margin: 0; }
  .row { display: flex; gap: 8px; flex-wrap: wrap; }
  .bar { display: flex; gap: 8px; padding: 10px 14px 6px; }
  .bar input { flex: 1; min-width: 0; }
  select {
    font: inherit; color: var(--text); background: var(--bg);
    border: 1px solid var(--border); border-radius: 6px; padding: 5px 8px;
  }
  .meta { padding: 0 14px 8px; font-size: 13px; }
  .muted { color: var(--muted); }
  .small { font-size: 12px; }
  .ok { color: var(--ok); white-space: nowrap; }
  .link { background: none; border: none; padding: 0; color: var(--accent); text-decoration: underline; }
  .results { flex: 1; min-height: 0; overflow: auto; border-top: 1px solid var(--border); }
  .empty { padding: 24px 16px; text-align: center; }
  table { width: 100%; border-collapse: collapse; font-variant-numeric: tabular-nums; }
  th {
    position: sticky; top: 0; background: var(--bg); color: var(--muted); font-weight: 500;
    text-align: left; padding: 6px 10px; border-bottom: 1px solid var(--border); white-space: nowrap;
  }
  .th { background: none; border: none; padding: 0; color: inherit; font: inherit; cursor: pointer; }
  td { padding: 6px 10px; border-bottom: 1px solid var(--border); white-space: nowrap; }
  td.name, th.name { width: 100%; max-width: 0; overflow: hidden; text-overflow: ellipsis; }
  .num { text-align: right; }
  th.num .th { text-align: right; }
  .seeds { color: var(--ok); }
  .seeds.dead { color: var(--muted); }
  .src { color: var(--muted); max-width: 140px; overflow: hidden; text-overflow: ellipsis; }
  .act button { padding: 3px 10px; }
  tbody tr { cursor: default; }
  tbody tr:hover { background: var(--row-hover); }
  tr.added td.name { color: var(--muted); }
</style>
