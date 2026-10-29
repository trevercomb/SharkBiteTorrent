<script lang="ts">
  import { GetFiles, SetFilesWanted, SetFilesHigh, SetFirstLast, ScanFully, MarkScanSeen } from '../wailsjs/go/main/App.js'
  import type { engine } from '../wailsjs/go/models'
  import { bytes } from './format'
  import PieceMap from './PieceMap.svelte'

  let { torrent, onError }: { torrent: engine.Status; onError: (e: string) => void } = $props()

  let files = $state<engine.FileStatus[]>([])
  const allWanted = $derived(files.length > 0 && files.every(f => f.wanted))
  const noneWanted = $derived(files.every(f => !f.wanted))
  const warnings = $derived(torrent.warnings ?? [])
  const flagged = $derived(new Set(warnings.map(w => w.file)))
  const tooLarge = $derived(torrent.tooLarge ?? [])

  // Opening the panel shows the "too large to scan" note, so the torrent's
  // row stops flagging it.
  $effect(() => {
    if (torrent.scanState === 'toolarge' && !torrent.scanSeen) MarkScanSeen(torrent.hash)
  })

  async function scanFully() {
    try {
      await ScanFully(torrent.hash)
    } catch (e) {
      onError(String(e))
    }
  }

  // Set when the panel closes or switches torrent, so a request still in
  // flight (e.g. for a torrent just removed) doesn't report an error.
  let stale = false

  async function load() {
    const h = hash
    try {
      const f = (await GetFiles(h)) ?? []
      if (!stale && h === hash) files = f
    } catch (e) {
      if (!stale && h === hash) onError(String(e))
    }
  }

  // Only restart when a different torrent is selected: the torrent object
  // itself is re-created on every 1s list refresh.
  const hash = $derived(torrent.hash)

  $effect(() => {
    void hash
    stale = false
    load()
    const id = setInterval(load, 1500)
    return () => {
      stale = true
      clearInterval(id)
    }
  })

  async function setWanted(indexes: number[], wanted: boolean) {
    try {
      await SetFilesWanted(hash, indexes, wanted)
      await load()
    } catch (e) {
      onError(String(e))
    }
  }

  async function setHigh(indexes: number[], high: boolean) {
    try {
      await SetFilesHigh(hash, indexes, high)
      await load()
    } catch (e) {
      onError(String(e))
    }
  }

  // "First and last pieces first": a video's start and its index at the end
  // come first, so it can be previewed while the rest downloads.
  async function setFirstLast(on: boolean) {
    try {
      await SetFirstLast(hash, on)
    } catch (e) {
      onError(String(e))
    }
  }
</script>

<section class="panel">
  <div class="head">
    <b title={torrent.name}>{torrent.name || torrent.hash}</b>
    <span class="muted">{files.length} file{files.length === 1 ? '' : 's'}</span>
    {#if files.length > 0 && torrent.state !== 'seeding'}
      <label class="firstlast" title="Each file's first and last pieces come first, so a video can be previewed while the rest downloads">
        <input type="checkbox" checked={torrent.firstLast} onchange={e => setFirstLast((e.target as HTMLInputElement).checked)} />
        First and last pieces first
      </label>
    {/if}
  </div>
  {#if warnings.length > 0 || torrent.scanState || torrent.verified || torrent.verifying || torrent.sfv || torrent.problem}
    <div class="safety">
      {#if torrent.problem}
        <div class="caution">⚠ Stopped: {torrent.problem}</div>
      {/if}
      {#each warnings as w (w.file)}
        <div class="caution">⚠ <b>{w.file}</b>: {w.reason}</div>
      {/each}
      {#if torrent.scanState === 'scanning'}
        <div class="muted">Scanning the finished files for malware…</div>
      {:else if torrent.scanState === 'clean'}
        {#if warnings.length > 0}
          <!-- No green tick under a warning: ClamAV passed a 1.2 GB fake-film .exe in full. -->
          <div class="muted">{torrent.scanEngine} found no known threats, but that doesn't make the file above safe: antivirus doesn't recognise all new malware.</div>
        {:else}
          <div class="ok">✓ {torrent.scanEngine} found no known threats.</div>
        {/if}
      {:else if torrent.scanState === 'threat'}
        <div class="threat">
          ⚠ {torrent.scanEngine} found: {(torrent.threats ?? []).join(', ')}. Don't open these files.
          Removing the torrent with its files is the safest option.
        </div>
      {:else if torrent.scanState === 'error'}
        <div class="muted">Not scanned for malware: {torrent.scanError}.</div>
      {:else if torrent.scanState === 'toolarge'}
        <div class="caution">
          ⚠ Couldn't scan fully for malware: {tooLarge.join(', ')}.
          {#if files.length > tooLarge.length}{torrent.scanEngine} found no known threats in the other files.{/if}
        </div>
        {#if torrent.canScanFully}
          <div>
            <button class="link" onclick={scanFully}>Scan fully</button>
            <span class="muted">(files up to 2 GB; can take a few minutes)</span>
          </div>
        {:else}
          <div class="muted">{torrent.scanEngine} stops at 2 GB of a file, counting what's packed inside it (like the system in a Linux ISO), and at very deeply nested archives.</div>
        {/if}
      {/if}
      {#if torrent.verifying}
        <div class="muted">Verifying files on disk against the torrent's piece hashes… {Math.floor(torrent.verifyProgress * 100)}%</div>
      {:else if torrent.verified}
        {#if torrent.verified.bad === 0}
          <div class="ok">
            ✓ Verified{torrent.verified.reason ? ' ' + torrent.verified.reason : ''}: all {torrent.verified.pieces} finished
            pieces on disk match the torrent.
          </div>
        {:else if torrent.state === 'seeding'}
          <div class="ok">
            ✓ Verified{torrent.verified.reason ? ' ' + torrent.verified.reason : ''}: {torrent.verified.bad} damaged
            {torrent.verified.bad === 1 ? 'piece was' : 'pieces were'} downloaded again
            ({(torrent.verified.badFiles ?? []).join(', ')}); all {torrent.verified.pieces} now match.
          </div>
        {:else}
          <div class="threat">
            ⚠ {torrent.verified.bad} of {torrent.verified.pieces} pieces didn't match
            ({(torrent.verified.badFiles ?? []).join(', ')}). They are being downloaded again.
          </div>
        {/if}
      {/if}
      {#if torrent.sfv?.checking}
        <div class="muted">Checking files against the torrent's .sfv…</div>
      {:else if torrent.sfv}
        {#if torrent.sfv.error}
          <div class="muted">Couldn't check the .sfv: {torrent.sfv.error}</div>
        {:else if (torrent.sfv.bad ?? []).length > 0}
          <div class="threat">⚠ .sfv: {torrent.sfv.bad.length} file(s) don't match: {torrent.sfv.bad.join(', ')}</div>
        {:else}
          <div class="ok">
            ✓ .sfv: {torrent.sfv.checked} file{torrent.sfv.checked === 1 ? '' : 's'} match{torrent.sfv.checked === 1 ? 'es' : ''}.
            {#if (torrent.sfv.missing ?? []).length > 0}<span class="muted">({torrent.sfv.missing.length} listed but not downloaded)</span>{/if}
          </div>
        {/if}
      {/if}
    </div>
  {/if}
  <PieceMap hash={torrent.hash} />
  {#if files.length === 0}
    <div class="empty">Waiting for metadata from peers…</div>
  {:else}
    <div class="list">
      <table>
        <thead>
          <tr>
            <th class="check">
              <input
                type="checkbox"
                checked={allWanted}
                indeterminate={!allWanted && !noneWanted}
                onchange={() => setWanted(files.map(f => f.index), !allWanted)}
                title="Select all"
              />
            </th>
            <th>File</th><th class="num">Size</th><th class="num">Done</th><th class="prio">Priority</th>
          </tr>
        </thead>
        <tbody>
          {#each files as f (f.index)}
            <tr class:skipped={!f.wanted}>
              <td class="check">
                <input type="checkbox" checked={f.wanted} onchange={() => setWanted([f.index], !f.wanted)} />
              </td>
              <td class="path" title={f.path}>
                {#if flagged.has(f.path)}<span class="flag">⚠</span>{/if}{f.path}
              </td>
              <td class="num">{bytes(f.size)}</td>
              <td class="num">{f.size ? ((f.done / f.size) * 100).toFixed(0) : 100}%</td>
              <td class="prio">
                {#if f.wanted && f.done < f.size}
                  <select value={f.high ? 'high' : 'normal'} onchange={e => setHigh([f.index], (e.target as HTMLSelectElement).value === 'high')}
                    title="High: downloaded before the other files">
                    <option value="normal">Normal</option>
                    <option value="high">High</option>
                  </select>
                {:else}<span class="muted">{f.wanted ? 'Done' : '–'}</span>{/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<style>
  .panel { display: flex; flex-direction: column; min-height: 0; height: 100%; }
  .head {
    display: flex; align-items: center; gap: 10px; padding: 8px 14px;
    border-bottom: 1px solid var(--border); background: var(--panel);
  }
  .head b { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 600; }
  .muted { color: var(--muted); white-space: nowrap; }
  .empty { padding: 16px; color: var(--muted); }
  .safety { padding: 8px 14px; border-bottom: 1px solid var(--border); display: grid; gap: 4px; font-size: 13px; user-select: text; }
  .safety .caution { color: var(--warn); }
  .safety .threat { color: var(--err); font-weight: 600; }
  .safety .ok { color: var(--ok); }
  .safety .link { background: none; border: none; padding: 0; color: var(--accent); text-decoration: underline; }
  .flag { color: var(--warn); font-weight: 700; margin-right: 5px; }
  .list { overflow: auto; flex: 1; }
  table { width: 100%; border-collapse: collapse; font-variant-numeric: tabular-nums; }
  th {
    position: sticky; top: 0; background: var(--bg); color: var(--muted); font-weight: 500;
    text-align: left; padding: 5px 10px; border-bottom: 1px solid var(--border);
  }
  td { padding: 4px 10px; border-bottom: 1px solid var(--border); white-space: nowrap; }
  .check { width: 32px; }
  .path { max-width: 0; width: 100%; overflow: hidden; text-overflow: ellipsis; }
  .num { text-align: right; }
  tr.skipped td:not(.check):not(.prio) { color: var(--muted); text-decoration: line-through; }
  .prio { width: 90px; }
  .prio select { padding: 1px 4px; font-size: 12px; }
  .firstlast { margin-left: auto; display: flex; align-items: center; gap: 6px; font-size: 12px; color: var(--muted); white-space: nowrap; cursor: pointer; }
</style>
