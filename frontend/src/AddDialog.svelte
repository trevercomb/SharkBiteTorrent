<script lang="ts">
  import { ChooseDirectory, SetAskSaveLocation } from '../wailsjs/go/main/App.js'
  import type { main } from '../wailsjs/go/models'

  // Asked before adding anything when Settings → Downloads → "Ask where to
  // save each download" is on.
  let { what, defaultDir, recentDirs, onConfirm, onCancel }: {
    what: string // e.g. the magnet's name, "3 torrent files", "8 links"
    defaultDir: string
    recentDirs: string[]
    onConfirm: (c: main.AddChoice) => void
    onCancel: () => void
  } = $props()

  // svelte-ignore state_referenced_locally
  let dir = $state(defaultDir) // starts at the default; the dialog is new each time
  let picked = $state<string[]>([]) // folders chosen with Browse… this time
  let paused = $state(false)
  let dontAsk = $state(false)

  const options = $derived([...new Set([defaultDir, ...picked, ...recentDirs])].filter(Boolean))

  async function browse() {
    const d = await ChooseDirectory(dir)
    if (d) {
      picked = [d, ...picked.filter(p => p !== d)]
      dir = d
    }
  }

  async function confirm() {
    if (dontAsk) await SetAskSaveLocation(false)
    onConfirm({ dir: dir === defaultDir ? '' : dir, paused } as main.AddChoice)
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape') onCancel()
  }
</script>

<svelte:window onkeydown={onKey} />

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
<div class="backdrop" onclick={onCancel}>
  <div class="dialog" role="dialog" aria-modal="true" aria-labelledby="add-title" tabindex="-1" onclick={e => e.stopPropagation()}>
    <h3 id="add-title">Add download</h3>
    <p class="what" title={what}>{what}</p>

    <label class="field">
      <span>Save to</span>
      <div class="row">
        <select bind:value={dir}>
          {#each options as d (d)}
            <option value={d}>{d}{d === defaultDir ? '  (default)' : ''}</option>
          {/each}
        </select>
        <button onclick={browse}>Browse…</button>
      </div>
    </label>

    <label class="check">
      <input type="checkbox" bind:checked={paused} />
      <span>Add paused (start it yourself later)</span>
    </label>

    <div class="actions">
      <label class="check muted">
        <input type="checkbox" bind:checked={dontAsk} />
        <span>Don't ask again</span>
      </label>
      <span class="grow"></span>
      <button onclick={onCancel}>Cancel</button>
      <button class="primary" onclick={confirm}>{paused ? 'Add paused' : 'Download'}</button>
    </div>
    {#if dontAsk}<p class="muted small">You can turn this back on in Settings → Downloads.</p>{/if}
  </div>
</div>

<style>
  .backdrop { position: fixed; inset: 0; background: rgba(0, 0, 0, 0.55); display: grid; place-items: center; z-index: 12; }
  .dialog {
    width: min(520px, calc(100vw - 32px)); padding: 18px 20px;
    background: var(--panel); border: 1px solid var(--border); border-radius: 10px;
    box-shadow: 0 20px 50px rgba(0, 0, 0, 0.5);
  }
  h3 { margin: 0 0 6px; font-size: 16px; font-weight: 600; }
  .what { margin: 0 0 14px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .field { display: flex; flex-direction: column; gap: 6px; margin-bottom: 12px; }
  .row { display: flex; gap: 8px; }
  select { flex: 1; min-width: 0; }
  .check { display: flex; align-items: center; gap: 8px; cursor: pointer; }
  .muted { color: var(--muted); }
  .small { font-size: 12px; margin: 8px 0 0; }
  .actions { display: flex; align-items: center; gap: 8px; margin-top: 18px; }
  .grow { flex: 1; }
</style>
